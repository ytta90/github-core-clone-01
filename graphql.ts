// GraphQL over POST /api/graphql.
//
// A small, dependency-free GraphQL: a tokenizer and recursive-descent parser
// for the documents the suite sends (queries, mutations, variables, input
// objects, lists, enums, aliases, inline fragments and named fragment
// spreads), and an executor over the same store the REST surface uses, so a
// mutation through GraphQL is visible through REST and the other way round.
//
// The contract the suite relies on: an unknown repo answers data.repository
// null with no errors; an unknown field answers HTTP 200 with an errors array
// naming the field; two operation documents in one body answer an array of
// responses in order; cursors are base64 and page connections forward.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { FIXED_ISO, diffTrees, mergeBase, parseCommit } from './store.ts';
import type { Issue, PullRequest, Repo, Store } from './store.ts';
import { isRecord, parseJsonBody, readBody, sendJson } from './http.ts';
import { pullConflicts, squashMerge } from './rest.ts';

const MAX_BODY = 16 * 1024 * 1024;

// ---------------------------------------------------------------------------------------------
// global ids: opaque base64 of "Kind:..." — never the plain database id.
// ---------------------------------------------------------------------------------------------

export function repoGlobalId(repo: Repo): string {
  return Buffer.from(`Repository:${repo.id}`, 'utf8').toString('base64');
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return Buffer.from(`Issue:${repo.id}:${issue.number}`, 'utf8').toString('base64');
}

export function pullGlobalId(repo: Repo, pull: PullRequest): string {
  return Buffer.from(`PullRequest:${repo.id}:${pull.number}`, 'utf8').toString('base64');
}

export function commentGlobalId(repo: Repo, issueNumber: number, commentId: number): string {
  return Buffer.from(`IssueComment:${repo.id}:${issueNumber}:${commentId}`, 'utf8').toString('base64');
}

interface DecodedId {
  readonly kind: string;
  readonly parts: string[];
}

function decodeGlobalId(id: string): DecodedId | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const parts = text.split(':');
    if (parts.length < 2) return null;
    return { kind: parts[0], parts: parts.slice(1) };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// tokenizer
// ---------------------------------------------------------------------------------------------

interface Token {
  readonly kind: 'name' | 'int' | 'float' | 'string' | 'punct' | 'variable' | 'eof';
  readonly value: string;
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let index = 0;
  const isNameStart = (ch: string): boolean => /[_A-Za-z]/.test(ch);
  const isNameChar = (ch: string): boolean => /[_0-9A-Za-z]/.test(ch);
  while (index < source.length) {
    const ch = source[index];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      index += 1;
      continue;
    }
    if (ch === '#') {
      while (index < source.length && source[index] !== '\n') index += 1;
      continue;
    }
    if (ch === '"') {
      // A block string (""") is read to its closing triple quote.
      if (source.startsWith('"""', index)) {
        const end = source.indexOf('"""', index + 3);
        const value = end < 0 ? source.slice(index + 3) : source.slice(index + 3, end);
        tokens.push({ kind: 'string', value });
        index = end < 0 ? source.length : end + 3;
        continue;
      }
      let value = '';
      index += 1;
      while (index < source.length && source[index] !== '"') {
        if (source[index] === '\\' && index + 1 < source.length) {
          const escaped = source[index + 1];
          if (escaped === 'n') value += '\n';
          else if (escaped === 't') value += '\t';
          else if (escaped === 'r') value += '\r';
          else value += escaped;
          index += 2;
          continue;
        }
        value += source[index];
        index += 1;
      }
      index += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (ch === '$') {
      let end = index + 1;
      while (end < source.length && isNameChar(source[end])) end += 1;
      tokens.push({ kind: 'variable', value: source.slice(index + 1, end) });
      index = end;
      continue;
    }
    if (isNameStart(ch)) {
      let end = index + 1;
      while (end < source.length && isNameChar(source[end])) end += 1;
      tokens.push({ kind: 'name', value: source.slice(index, end) });
      index = end;
      continue;
    }
    if (ch === '-' || /[0-9]/.test(ch)) {
      let end = index;
      if (source[end] === '-') end += 1;
      while (end < source.length && /[0-9]/.test(source[end])) end += 1;
      let isFloat = false;
      if (source[end] === '.' && /[0-9]/.test(source[end + 1] ?? '')) {
        isFloat = true;
        end += 1;
        while (end < source.length && /[0-9]/.test(source[end])) end += 1;
      }
      if (source[end] === 'e' || source[end] === 'E') {
        isFloat = true;
        end += 1;
        if (source[end] === '+' || source[end] === '-') end += 1;
        while (end < source.length && /[0-9]/.test(source[end])) end += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: source.slice(index, end) });
      index = end;
      continue;
    }
    if (source.startsWith('...', index)) {
      tokens.push({ kind: 'punct', value: '...' });
      index += 3;
      continue;
    }
    tokens.push({ kind: 'punct', value: ch });
    index += 1;
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

type ValueNode =
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'variable'; name: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

interface FieldNode {
  readonly kind: 'field';
  readonly name: string;
  readonly alias: string | null;
  readonly args: Array<{ name: string; value: ValueNode }>;
  readonly selections: SelectionNode[] | null;
}

type SelectionNode =
  | FieldNode
  | { kind: 'spread'; name: string }
  | { kind: 'inline'; typeCondition: string | null; selections: SelectionNode[] };

interface OperationNode {
  readonly kind: 'operation';
  readonly operation: 'query' | 'mutation';
  readonly name: string | null;
  readonly variables: Array<{ name: string; defaultValue: ValueNode | null }>;
  readonly selections: SelectionNode[];
}

interface FragmentNode {
  readonly kind: 'fragment';
  readonly name: string;
  readonly typeCondition: string;
  readonly selections: SelectionNode[];
}

interface DocumentNode {
  readonly operations: OperationNode[];
  readonly fragments: Map<string, FragmentNode>;
}

class Parser {
  private position = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.position];
  }

  private next(): Token {
    const token = this.tokens[this.position];
    if (this.position < this.tokens.length - 1) this.position += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected ${value}, got ${token.value}`);
  }

  private expectName(value?: string): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error(`expected a name, got ${token.value}`);
    if (value !== undefined && token.value !== value) throw new Error(`expected ${value}, got ${token.value}`);
    return token.value;
  }

  private skipDirectives(): void {
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.expectName();
      if (this.peek().kind === 'punct' && this.peek().value === '(') this.parseArguments();
    }
  }

  parseDocument(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();
    while (this.peek().kind !== 'eof') {
      if (this.peek().kind === 'name' && this.peek().value === 'fragment') {
        const fragment = this.parseFragment();
        fragments.set(fragment.name, fragment);
        continue;
      }
      operations.push(this.parseOperation());
    }
    return { operations, fragments };
  }

  private parseFragment(): FragmentNode {
    this.expectName('fragment');
    const name = this.expectName();
    this.expectName('on');
    const typeCondition = this.expectName();
    this.skipDirectives();
    return { kind: 'fragment', name, typeCondition, selections: this.parseSelectionSet() };
  }

  private parseOperation(): OperationNode {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '{') {
      return { kind: 'operation', operation: 'query', name: null, variables: [], selections: this.parseSelectionSet() };
    }
    const operation = this.expectName();
    if (operation !== 'query' && operation !== 'mutation') throw new Error(`unsupported operation ${operation}`);
    let name: string | null = null;
    if (this.peek().kind === 'name') name = this.next().value;
    const variables: Array<{ name: string; defaultValue: ValueNode | null }> = [];
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        const varToken = this.next();
        if (varToken.kind !== 'variable') throw new Error('expected a variable in the definition list');
        this.expectPunct(':');
        this.parseType();
        let defaultValue: ValueNode | null = null;
        if (this.peek().kind === 'punct' && this.peek().value === '=') {
          this.next();
          defaultValue = this.parseValue();
        }
        variables.push({ name: varToken.value, defaultValue });
      }
      this.expectPunct(')');
    }
    this.skipDirectives();
    return { kind: 'operation', operation, name, variables, selections: this.parseSelectionSet() };
  }

  private parseType(): void {
    if (this.peek().kind === 'punct' && this.peek().value === '[') {
      this.next();
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    if (this.peek().kind === 'punct' && this.peek().value === '!') this.next();
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '...') {
      this.next();
      const next = this.peek();
      if (next.kind === 'name' && next.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        this.skipDirectives();
        return { kind: 'inline', typeCondition, selections: this.parseSelectionSet() };
      }
      if (next.kind === 'name') {
        const name = this.next().value;
        this.skipDirectives();
        return { kind: 'spread', name };
      }
      this.skipDirectives();
      return { kind: 'inline', typeCondition: null, selections: this.parseSelectionSet() };
    }
    return this.parseField();
  }

  private parseField(): FieldNode {
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args = this.peek().kind === 'punct' && this.peek().value === '(' ? this.parseArguments() : [];
    this.skipDirectives();
    const selections = this.peek().kind === 'punct' && this.peek().value === '{' ? this.parseSelectionSet() : null;
    return { kind: 'field', name, alias, args, selections };
  }

  private parseArguments(): Array<{ name: string; value: ValueNode }> {
    this.expectPunct('(');
    const args: Array<{ name: string; value: ValueNode }> = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
      const name = this.expectName();
      this.expectPunct(':');
      args.push({ name, value: this.parseValue() });
    }
    this.expectPunct(')');
    return args;
  }

  private parseValue(): ValueNode {
    const token = this.next();
    if (token.kind === 'variable') return { kind: 'variable', name: token.value };
    if (token.kind === 'int') return { kind: 'int', value: Number.parseInt(token.value, 10) };
    if (token.kind === 'float') return { kind: 'float', value: Number.parseFloat(token.value) };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueNode[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) values.push(this.parseValue());
      this.expectPunct(']');
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    throw new Error(`unexpected token ${token.value} in a value`);
  }
}

function evaluateValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
    case 'enum':
      return node.value;
    case 'null':
      return null;
    case 'variable':
      return variables[node.name] ?? null;
    case 'list':
      return node.values.map((value) => evaluateValue(value, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = evaluateValue(field.value, variables);
      return out;
    }
  }
}

// ---------------------------------------------------------------------------------------------
// the schema the introspection answers describe
// ---------------------------------------------------------------------------------------------

const SCHEMA_FIELDS: Readonly<Record<string, readonly string[]>> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['id', 'login', 'name', 'email'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest',
  ],
  Issue: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'createdAt', 'updatedAt', 'repository', 'comments'],
  PullRequest: [
    'id', 'databaseId', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'mergedAt',
    'mergeCommit', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'files', 'repository',
  ],
  IssueComment: ['id', 'databaseId', 'body', 'createdAt', 'updatedAt', 'author'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueCommentConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  BranchRef: ['name'],
  IssueEdge: ['node', 'cursor'],
  PullRequestEdge: ['node', 'cursor'],
  IssueCommentEdge: ['node', 'cursor'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'subject', 'clientMutationId'],
};

const SCHEMA_ENUMS: Readonly<Record<string, readonly string[]>> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'MILESTONE', 'ORGANIZATION'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
  PullRequestOrderField: ['CREATED_AT', 'UPDATED_AT'],
};

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

interface GraphContext {
  readonly store: Store;
  readonly login: string;
}

/** A resolved object: its GraphQL type plus the record it wraps. */
interface NodeValue {
  readonly __typename: string;
  readonly [key: string]: unknown;
}

type Resolver = (parent: NodeValue, args: Record<string, unknown>, ctx: GraphContext) => unknown;

function repoOf(ctx: GraphContext, node: NodeValue): Repo | null {
  const repo = node.__repo as Repo | undefined;
  return repo ?? null;
}

function findRepoById(ctx: GraphContext, id: number): Repo | null {
  for (const repo of ctx.store.repos.values()) if (repo.id === id) return repo;
  return null;
}

function issueNode(repo: Repo, issue: Issue): NodeValue {
  return { __typename: 'Issue', __repo: repo, __issue: issue };
}

function pullNode(repo: Repo, pull: PullRequest): NodeValue {
  return { __typename: 'PullRequest', __repo: repo, __pull: pull };
}

function commentNode(repo: Repo, issueNumber: number, comment: { id: number; body: string; createdAt: string; updatedAt: string }): NodeValue {
  return { __typename: 'IssueComment', __repo: repo, __issueNumber: issueNumber, __comment: comment };
}

function connection<T>(items: readonly T[], args: Record<string, unknown>, cursorOf: (item: T) => string): NodeValue {
  const after = typeof args.after === 'string' ? args.after : null;
  const first = typeof args.first === 'number' ? args.first : null;
  let start = 0;
  if (after !== null) {
    const index = items.findIndex((item) => cursorOf(item) === after);
    if (index >= 0) start = index + 1;
  }
  const page = first === null ? items.slice(start) : items.slice(start, start + first);
  const endCursor = page.length === 0 ? null : cursorOf(page[page.length - 1]);
  const startCursor = page.length === 0 ? null : cursorOf(page[0]);
  const hasNextPage = start + page.length < items.length;
  return {
    __typename: 'Connection',
    __items: page,
    __edges: page.map((item) => ({ __typename: 'Edge', __node: item, __cursor: cursorOf(item) })),
    __pageInfo: { hasNextPage, hasPreviousPage: start > 0, startCursor, endCursor },
    __totalCount: items.length,
  };
}

function cursorForNumber(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function orderedIssues(repo: Repo, args: Record<string, unknown>): Issue[] {
  let issues = repo.issueOrder.map((number) => repo.issues.get(number)!);
  const states = args.states;
  if (Array.isArray(states) && states.length > 0) {
    const wanted = new Set(states.map((state) => String(state).toLowerCase()));
    issues = issues.filter((issue) => wanted.has(issue.state));
  }
  const orderBy = isRecord(args.orderBy) ? args.orderBy : null;
  const field = orderBy !== null && typeof orderBy.field === 'string' ? orderBy.field : 'CREATED_AT';
  const direction = orderBy !== null && typeof orderBy.direction === 'string' ? orderBy.direction : 'ASC';
  if (field === 'CREATED_AT') {
    issues.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
  }
  return issues;
}

const RESOLVERS: Readonly<Record<string, Readonly<Record<string, Resolver>>>> = {
  Query: {
    viewer: (_parent, _args, ctx) => ({ __typename: 'User', __login: ctx.login }),
    repository: (_parent, args, ctx) => {
      const owner = typeof args.owner === 'string' ? args.owner : '';
      const name = typeof args.name === 'string' ? args.name : '';
      if (owner !== ctx.login) return null;
      const repo = ctx.store.getRepo(owner, name);
      return repo === undefined ? null : { __typename: 'Repository', __repo: repo };
    },
    node: (_parent, args, ctx) => {
      const id = typeof args.id === 'string' ? args.id : '';
      const decoded = decodeGlobalId(id);
      if (decoded === null) return null;
      if (decoded.kind === 'Repository') {
        const repo = findRepoById(ctx, Number(decoded.parts[0]));
        return repo === null ? null : { __typename: 'Repository', __repo: repo };
      }
      if (decoded.kind === 'Issue') {
        const repo = findRepoById(ctx, Number(decoded.parts[0]));
        if (repo === null) return null;
        const issue = repo.issues.get(Number(decoded.parts[1]));
        return issue === undefined ? null : issueNode(repo, issue);
      }
      if (decoded.kind === 'PullRequest') {
        const repo = findRepoById(ctx, Number(decoded.parts[0]));
        if (repo === null) return null;
        const pull = repo.pulls.get(Number(decoded.parts[1]));
        return pull === undefined ? null : pullNode(repo, pull);
      }
      if (decoded.kind === 'IssueComment') {
        const repo = findRepoById(ctx, Number(decoded.parts[0]));
        if (repo === null) return null;
        const issue = repo.issues.get(Number(decoded.parts[1]));
        if (issue === undefined) return null;
        const comment = issue.comments.find((candidate) => candidate.id === Number(decoded.parts[2]));
        return comment === undefined ? null : commentNode(repo, issue.number, comment);
      }
      return null;
    },
    __type: (_parent, args) => {
      const name = typeof args.name === 'string' ? args.name : '';
      if (SCHEMA_FIELDS[name] !== undefined) {
        return {
          __typename: '__Type',
          __typeName: name,
          __kind: 'OBJECT',
          __fields: SCHEMA_FIELDS[name].map((field) => ({ name: field })),
          __enumValues: null,
        };
      }
      if (SCHEMA_ENUMS[name] !== undefined) {
        return {
          __typename: '__Type',
          __typeName: name,
          __kind: 'ENUM',
          __fields: null,
          __enumValues: SCHEMA_ENUMS[name].map((value) => ({ name: value })),
        };
      }
      return null;
    },
    __schema: () => ({ __typename: '__Schema' }),
  },
  Mutation: {
    createIssue: (_parent, args, ctx) => {
      const input = isRecord(args.input) ? args.input : {};
      const repositoryId = typeof input.repositoryId === 'string' ? input.repositoryId : '';
      const decoded = decodeGlobalId(repositoryId);
      if (decoded === null || decoded.kind !== 'Repository') return { __typename: 'CreateIssuePayload', __issue: null };
      const repo = findRepoById(ctx, Number(decoded.parts[0]));
      if (repo === null) return { __typename: 'CreateIssuePayload', __issue: null };
      const title = typeof input.title === 'string' ? input.title : '';
      const number = repo.nextIssueNumber++;
      const issue: Issue = {
        id: repo.id * 1_000_000 + number,
        number,
        title,
        body: typeof input.body === 'string' ? input.body : '',
        state: 'open',
        labels: [],
        comments: [],
        createdAt: FIXED_ISO,
        updatedAt: FIXED_ISO,
      };
      repo.issues.set(number, issue);
      repo.issueOrder.push(number);
      return { __typename: 'CreateIssuePayload', __issue: issueNode(repo, issue), __clientMutationId: input.clientMutationId ?? null };
    },
    closeIssue: (_parent, args, ctx) => {
      const input = isRecord(args.input) ? args.input : {};
      const issueId = typeof input.issueId === 'string' ? input.issueId : '';
      const decoded = decodeGlobalId(issueId);
      if (decoded === null || decoded.kind !== 'Issue') return { __typename: 'CloseIssuePayload', __issue: null };
      const repo = findRepoById(ctx, Number(decoded.parts[0]));
      if (repo === null) return { __typename: 'CloseIssuePayload', __issue: null };
      const issue = repo.issues.get(Number(decoded.parts[1]));
      if (issue === undefined) return { __typename: 'CloseIssuePayload', __issue: null };
      issue.state = 'closed';
      return { __typename: 'CloseIssuePayload', __issue: issueNode(repo, issue), __clientMutationId: input.clientMutationId ?? null };
    },
    createPullRequest: (_parent, args, ctx) => {
      const input = isRecord(args.input) ? args.input : {};
      const repositoryId = typeof input.repositoryId === 'string' ? input.repositoryId : '';
      const decoded = decodeGlobalId(repositoryId);
      if (decoded === null || decoded.kind !== 'Repository') return { __typename: 'CreatePullRequestPayload', __pull: null };
      const repo = findRepoById(ctx, Number(decoded.parts[0]));
      if (repo === null) return { __typename: 'CreatePullRequestPayload', __pull: null };
      const headRef = typeof input.headRefName === 'string' ? input.headRefName : '';
      const baseRef = typeof input.baseRefName === 'string' ? input.baseRefName : '';
      const headTip = repo.refs.get(`refs/heads/${headRef}`) ?? null;
      const baseTip = repo.refs.get(`refs/heads/${baseRef}`) ?? null;
      if (headTip === null || baseTip === null) return { __typename: 'CreatePullRequestPayload', __pull: null };
      for (const existing of repo.pulls.values()) {
        if (existing.state === 'open' && existing.headRef === headRef && existing.baseRef === baseRef) {
          return { __typename: 'CreatePullRequestPayload', __pull: pullNode(repo, existing), __clientMutationId: input.clientMutationId ?? null };
        }
      }
      const number = repo.nextPullNumber++;
      const pull: PullRequest = {
        id: repo.id * 1_000_000 + number,
        number,
        title: typeof input.title === 'string' ? input.title : '',
        body: typeof input.body === 'string' ? input.body : '',
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        headRef,
        baseRef,
        headSha: headTip,
        baseSha: baseTip,
        createdAt: FIXED_ISO,
        updatedAt: FIXED_ISO,
      };
      repo.pulls.set(number, pull);
      repo.pullOrder.push(number);
      return { __typename: 'CreatePullRequestPayload', __pull: pullNode(repo, pull), __clientMutationId: input.clientMutationId ?? null };
    },
    mergePullRequest: (_parent, args, ctx) => {
      const input = isRecord(args.input) ? args.input : {};
      const pullRequestId = typeof input.pullRequestId === 'string' ? input.pullRequestId : '';
      const decoded = decodeGlobalId(pullRequestId);
      if (decoded === null || decoded.kind !== 'PullRequest') return { __typename: 'MergePullRequestPayload', __pull: null };
      const repo = findRepoById(ctx, Number(decoded.parts[0]));
      if (repo === null) return { __typename: 'MergePullRequestPayload', __pull: null };
      const pull = repo.pulls.get(Number(decoded.parts[1]));
      if (pull === undefined) return { __typename: 'MergePullRequestPayload', __pull: null };
      if (!pull.merged && !pullConflicts(repo, pull)) {
        const sha = squashMerge(repo, ctx, pull);
        pull.merged = true;
        pull.state = 'closed';
        pull.mergedAt = FIXED_ISO;
        pull.mergeCommitSha = sha;
      }
      return { __typename: 'MergePullRequestPayload', __pull: pullNode(repo, pull), __clientMutationId: input.clientMutationId ?? null };
    },
    addComment: (_parent, args, ctx) => {
      const input = isRecord(args.input) ? args.input : {};
      const subjectId = typeof input.subjectId === 'string' ? input.subjectId : '';
      const decoded = decodeGlobalId(subjectId);
      if (decoded === null || decoded.kind !== 'Issue') return { __typename: 'AddCommentPayload', __commentEdge: null };
      const repo = findRepoById(ctx, Number(decoded.parts[0]));
      if (repo === null) return { __typename: 'AddCommentPayload', __commentEdge: null };
      const issue = repo.issues.get(Number(decoded.parts[1]));
      if (issue === undefined) return { __typename: 'AddCommentPayload', __commentEdge: null };
      const comment = {
        id: repo.nextCommentId++,
        body: typeof input.body === 'string' ? input.body : '',
        createdAt: FIXED_ISO,
        updatedAt: FIXED_ISO,
      };
      issue.comments.push(comment);
      return {
        __typename: 'AddCommentPayload',
        __commentEdge: { __typename: 'IssueCommentEdge', __node: commentNode(repo, issue.number, comment), __cursor: cursorForNumber(comment.id) },
        __subject: issueNode(repo, issue),
        __clientMutationId: input.clientMutationId ?? null,
      };
    },
  },
  User: {
    id: (parent, _args, ctx) => Buffer.from(`User:${ctx.login}`, 'utf8').toString('base64'),
    login: (parent) => String(parent.__login ?? ''),
    name: (parent) => String(parent.__login ?? ''),
    email: (parent) => `${String(parent.__login ?? '')}@example.invalid`,
  },
  Repository: {
    id: (parent) => repoGlobalId(repoOfCurrent(parent)),
    databaseId: (parent) => repoOfCurrent(parent).id,
    name: (parent) => repoOfCurrent(parent).name,
    nameWithOwner: (parent) => `${repoOfCurrent(parent).owner}/${repoOfCurrent(parent).name}`,
    owner: (parent, _args, ctx) => ({ __typename: 'User', __login: repoOfCurrent(parent).owner }),
    defaultBranchRef: (parent) => {
      const repo = repoOfCurrent(parent);
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
      return tip === null ? null : { __typename: 'BranchRef', __name: repo.defaultBranch };
    },
    viewerPermission: () => 'ADMIN',
    hasIssuesEnabled: () => true,
    issue: (parent, args) => {
      const repo = repoOfCurrent(parent);
      const number = typeof args.number === 'number' ? args.number : -1;
      const issue = repo.issues.get(number);
      return issue === undefined ? null : issueNode(repo, issue);
    },
    issues: (parent, args) => {
      const repo = repoOfCurrent(parent);
      const issues = orderedIssues(repo, args);
      const nodes = issues.map((issue) => issueNode(repo, issue));
      return connection(nodes, args, (node) => cursorForNumber(issueOf(node).number));
    },
    pullRequests: (parent, args) => {
      const repo = repoOfCurrent(parent);
      const pulls = repo.pullOrder.map((number) => repo.pulls.get(number)!);
      const nodes = pulls.map((pull) => pullNode(repo, pull));
      return connection(nodes, args, (node) => cursorForNumber(pullOf(node).number));
    },
    pullRequest: (parent, args) => {
      const repo = repoOfCurrent(parent);
      const number = typeof args.number === 'number' ? args.number : -1;
      const pull = repo.pulls.get(number);
      return pull === undefined ? null : pullNode(repo, pull);
    },
  },
  Issue: {
    id: (parent) => issueGlobalId(repoOfCurrent(parent), issueOf(parent)),
    databaseId: (parent) => issueOf(parent).number,
    number: (parent) => issueOf(parent).number,
    title: (parent) => issueOf(parent).title,
    body: (parent) => issueOf(parent).body,
    state: (parent) => issueOf(parent).state.toUpperCase(),
    createdAt: (parent) => issueOf(parent).createdAt,
    updatedAt: (parent) => issueOf(parent).updatedAt,
    repository: (parent) => ({ __typename: 'Repository', __repo: repoOfCurrent(parent) }),
    comments: (parent, args) => {
      const repo = repoOfCurrent(parent);
      const issue = issueOf(parent);
      const nodes = issue.comments.map((comment) => commentNode(repo, issue.number, comment));
      return connection(nodes, args, (node) => cursorForNumber(commentOf(node).id));
    },
  },
  PullRequest: {
    id: (parent) => pullGlobalId(repoOfCurrent(parent), pullOf(parent)),
    databaseId: (parent) => pullOf(parent).number,
    number: (parent) => pullOf(parent).number,
    title: (parent) => pullOf(parent).title,
    body: (parent) => pullOf(parent).body,
    state: (parent) => (pullOf(parent).merged ? 'CLOSED' : pullOf(parent).state.toUpperCase()),
    merged: (parent) => pullOf(parent).merged,
    mergeable: (parent) => (pullConflicts(repoOfCurrent(parent), pullOf(parent)) ? 'CONFLICTING' : 'MERGEABLE'),
    mergedAt: (parent) => pullOf(parent).mergedAt,
    mergeCommit: (parent) => {
      const pull = pullOf(parent);
      if (pull.mergeCommitSha === null) return null;
      return { __typename: 'Commit', __oid: pull.mergeCommitSha };
    },
    headRefName: (parent) => pullOf(parent).headRef,
    baseRefName: (parent) => pullOf(parent).baseRef,
    headRefOid: (parent) => pullOf(parent).headSha,
    baseRefOid: (parent) => pullOf(parent).baseSha,
    files: (parent, args) => {
      const repo = repoOfCurrent(parent);
      const pull = pullOf(parent);
      const headCommit = repo.objects.get(pull.headSha);
      if (headCommit === undefined || headCommit.type !== 'commit') {
        return connection([], args, () => '');
      }
      const headTree = parseCommit(headCommit.data).tree;
      const base = mergeBase(repo.objects, pull.baseSha, pull.headSha);
      const baseTree = base === null ? null : parseCommit(repo.objects.get(base)!.data).tree;
      const changes = diffTrees(repo, baseTree, headTree).map((change) => ({
        __typename: 'PullRequestChangedFile',
        __path: change.path,
        __additions: change.additions,
        __deletions: change.deletions,
        __changeType: change.status === 'added' ? 'ADDED' : change.status === 'deleted' ? 'REMOVED' : 'MODIFIED',
      }));
      return connection(changes, args, (change) => Buffer.from(`file:${change.__path}`, 'utf8').toString('base64'));
    },
    repository: (parent) => ({ __typename: 'Repository', __repo: repoOfCurrent(parent) }),
  },
  IssueComment: {
    id: (parent) => commentGlobalId(repoOfCurrent(parent), Number(parent.__issueNumber), commentOf(parent).id),
    databaseId: (parent) => commentOf(parent).id,
    body: (parent) => commentOf(parent).body,
    createdAt: (parent) => commentOf(parent).createdAt,
    updatedAt: (parent) => commentOf(parent).updatedAt,
    author: (_parent, _args, ctx) => ({ __typename: 'User', __login: ctx.login }),
  },
  IssueConnection: {
    nodes: (parent) => (parent.__items as unknown[]) ?? [],
    edges: (parent) => (parent.__edges as unknown[]) ?? [],
    pageInfo: (parent) => ({ __typename: 'PageInfo', ...(parent.__pageInfo as Record<string, unknown>) }),
    totalCount: (parent) => Number(parent.__totalCount ?? 0),
  },
  PullRequestConnection: {
    nodes: (parent) => (parent.__items as unknown[]) ?? [],
    edges: (parent) => (parent.__edges as unknown[]) ?? [],
    pageInfo: (parent) => ({ __typename: 'PageInfo', ...(parent.__pageInfo as Record<string, unknown>) }),
    totalCount: (parent) => Number(parent.__totalCount ?? 0),
  },
  IssueCommentConnection: {
    nodes: (parent) => (parent.__items as unknown[]) ?? [],
    edges: (parent) => (parent.__edges as unknown[]) ?? [],
    pageInfo: (parent) => ({ __typename: 'PageInfo', ...(parent.__pageInfo as Record<string, unknown>) }),
    totalCount: (parent) => Number(parent.__totalCount ?? 0),
  },
  PullRequestChangedFileConnection: {
    nodes: (parent) => (parent.__items as unknown[]) ?? [],
    edges: (parent) => (parent.__edges as unknown[]) ?? [],
    pageInfo: (parent) => ({ __typename: 'PageInfo', ...(parent.__pageInfo as Record<string, unknown>) }),
    totalCount: (parent) => Number(parent.__totalCount ?? 0),
  },
  Edge: {
    node: (parent) => (parent.__node as NodeValue) ?? null,
    cursor: (parent) => String(parent.__cursor ?? ''),
  },
  PullRequestChangedFile: {
    path: (parent) => String(parent.__path ?? ''),
    additions: (parent) => Number(parent.__additions ?? 0),
    deletions: (parent) => Number(parent.__deletions ?? 0),
    changeType: (parent) => String(parent.__changeType ?? 'MODIFIED'),
  },
  PageInfo: {
    hasNextPage: (parent) => Boolean(parent.hasNextPage),
    hasPreviousPage: (parent) => Boolean(parent.hasPreviousPage),
    startCursor: (parent) => (parent.startCursor as string | null) ?? null,
    endCursor: (parent) => (parent.endCursor as string | null) ?? null,
  },
  BranchRef: {
    name: (parent) => String(parent.__name ?? ''),
  },
  IssueCommentEdge: {
    node: (parent) => (parent.__node as NodeValue) ?? null,
    cursor: (parent) => String(parent.__cursor ?? ''),
  },
  CreateIssuePayload: {
    issue: (parent) => (parent.__issue as NodeValue | null) ?? null,
    clientMutationId: (parent) => (parent.__clientMutationId as string | null) ?? null,
  },
  CloseIssuePayload: {
    issue: (parent) => (parent.__issue as NodeValue | null) ?? null,
    clientMutationId: (parent) => (parent.__clientMutationId as string | null) ?? null,
  },
  CreatePullRequestPayload: {
    pullRequest: (parent) => (parent.__pull as NodeValue | null) ?? null,
    clientMutationId: (parent) => (parent.__clientMutationId as string | null) ?? null,
  },
  MergePullRequestPayload: {
    pullRequest: (parent) => (parent.__pull as NodeValue | null) ?? null,
    clientMutationId: (parent) => (parent.__clientMutationId as string | null) ?? null,
  },
  AddCommentPayload: {
    commentEdge: (parent) => (parent.__commentEdge as NodeValue | null) ?? null,
    subject: (parent) => (parent.__subject as NodeValue | null) ?? null,
    clientMutationId: (parent) => (parent.__clientMutationId as string | null) ?? null,
  },
  __Type: {
    name: (parent) => String(parent.__typeName ?? ''),
    kind: (parent) => String(parent.__kind ?? 'OBJECT'),
    fields: (parent) => (parent.__fields as Array<{ name: string }> | null) ?? null,
    enumValues: (parent) => (parent.__enumValues as Array<{ name: string }> | null) ?? null,
  },
  __Schema: {
    queryType: () => ({ __typename: '__Type', __typeName: 'Query', __kind: 'OBJECT', __fields: null, __enumValues: null }),
    mutationType: () => ({ __typename: '__Type', __typeName: 'Mutation', __kind: 'OBJECT', __fields: null, __enumValues: null }),
    types: () => Object.keys(SCHEMA_FIELDS).map((name) => ({ __typename: '__Type', __typeName: name, __kind: 'OBJECT', __fields: null, __enumValues: null })),
  },
  Commit: {
    oid: (parent) => String(parent.__oid ?? ''),
  },
};

function repoOfCurrent(parent: NodeValue): Repo {
  const repo = parent.__repo as Repo | undefined;
  if (repo === undefined) throw new Error('a resolver ran without its repository');
  return repo;
}

function issueOf(parent: NodeValue): Issue {
  const issue = parent.__issue as Issue | undefined;
  if (issue === undefined) throw new Error('a resolver ran without its issue');
  return issue;
}

function pullOf(parent: NodeValue): PullRequest {
  const pull = parent.__pull as PullRequest | undefined;
  if (pull === undefined) throw new Error('a resolver ran without its pull request');
  return pull;
}

function commentOf(parent: NodeValue): { id: number; body: string; createdAt: string; updatedAt: string } {
  const comment = parent.__comment as { id: number; body: string; createdAt: string; updatedAt: string } | undefined;
  if (comment === undefined) throw new Error('a resolver ran without its comment');
  return comment;
}

interface ExecutionError {
  readonly message: string;
}

/** Expand fragments in a selection list, keeping only the ones that apply here. */
function expandSelections(
  selections: readonly SelectionNode[],
  fragments: Map<string, FragmentNode>,
  typeName: string,
  depth = 0,
): FieldNode[] {
  const out: FieldNode[] = [];
  if (depth > 10) return out;
  for (const selection of selections) {
    if (selection.kind === 'field') {
      out.push(selection);
    } else if (selection.kind === 'spread') {
      const fragment = fragments.get(selection.name);
      if (fragment !== undefined) out.push(...expandSelections(fragment.selections, fragments, typeName, depth + 1));
    } else if (selection.kind === 'inline') {
      if (selection.typeCondition === null || selection.typeCondition === typeName) {
        out.push(...expandSelections(selection.selections, fragments, typeName, depth + 1));
      }
    }
  }
  return out;
}

/** Resolve one object's fields. Unknown fields become errors and null values. */
function resolveFields(
  typeName: string,
  value: NodeValue,
  selections: readonly SelectionNode[],
  variables: Record<string, unknown>,
  fragments: Map<string, FragmentNode>,
  errors: ExecutionError[],
  ctx: GraphContext,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const fields = expandSelections(selections, fragments, typeName);
  for (const field of fields) {
    const responseKey = field.alias ?? field.name;
    if (field.name === '__typename') {
      out[responseKey] = typeName;
      continue;
    }
    const typeResolvers = RESOLVERS[typeName];
    const resolver = typeResolvers?.[field.name];
    if (resolver === undefined) {
      errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".` });
      out[responseKey] = null;
      continue;
    }
    const args: Record<string, unknown> = {};
    for (const arg of field.args) args[arg.name] = evaluateValue(arg.value, variables);
    let resolved: unknown;
    try {
      resolved = resolver(value, args, ctx);
    } catch (error) {
      errors.push({ message: error instanceof Error ? error.message : 'resolver failed' });
      out[responseKey] = null;
      continue;
    }
    if (field.selections === null || field.selections.length === 0) {
      out[responseKey] = resolved;
      continue;
    }
    if (resolved === null || resolved === undefined) {
      out[responseKey] = null;
      continue;
    }
    if (Array.isArray(resolved)) {
      out[responseKey] = resolved.map((item) => {
        if (item !== null && typeof item === 'object' && '__typename' in item) {
          return resolveFields((item as NodeValue).__typename, item as NodeValue, field.selections!, variables, fragments, errors, ctx);
        }
        return item;
      });
      continue;
    }
    if (typeof resolved === 'object' && '__typename' in (resolved as Record<string, unknown>)) {
      const node = resolved as NodeValue;
      // A connection wrapper carries its items under __items; its type is set
      // by the field that produced it, so resolve it as that connection type.
      const childType = node.__typename === 'Connection' ? connectionTypeFor(field.name) : node.__typename;
      out[responseKey] = resolveFields(childType, node, field.selections, variables, fragments, errors, ctx);
      continue;
    }
    out[responseKey] = resolved;
  }
  return out;
}

function connectionTypeFor(fieldName: string): string {
  if (fieldName === 'issues' || fieldName === 'comments') return fieldName === 'issues' ? 'IssueConnection' : 'IssueCommentConnection';
  if (fieldName === 'pullRequests') return 'PullRequestConnection';
  if (fieldName === 'files') return 'PullRequestChangedFileConnection';
  return 'IssueConnection';
}

function executeOperation(
  operation: OperationNode,
  document: DocumentNode,
  variables: Record<string, unknown>,
  ctx: GraphContext,
): { data: Record<string, unknown> | null; errors?: ExecutionError[] } {
  const mergedVariables: Record<string, unknown> = { ...variables };
  for (const definition of operation.variables) {
    if (mergedVariables[definition.name] === undefined && definition.defaultValue !== null) {
      mergedVariables[definition.name] = evaluateValue(definition.defaultValue, mergedVariables);
    }
  }
  const rootType = operation.operation === 'mutation' ? 'Mutation' : 'Query';
  const root: NodeValue = { __typename: rootType };
  const errors: ExecutionError[] = [];
  const data = resolveFields(rootType, root, operation.selections, mergedVariables, document.fragments, errors, ctx);
  if (errors.length > 0) return { data, errors };
  return { data };
}

// ---------------------------------------------------------------------------------------------
// HTTP entry point
// ---------------------------------------------------------------------------------------------

/** Answer one POST /api/graphql request (single operation or a batch array). */
export async function handleGraphql(store: Store, login: string, req: IncomingMessage, res: ServerResponse): Promise<void> {
  let body: Buffer;
  try {
    body = await readBody(req, MAX_BODY);
  } catch {
    sendJson(res, 400, { message: 'request body too large' });
    return;
  }
  let parsed: unknown;
  try {
    parsed = parseJsonBody(body);
  } catch {
    sendJson(res, 400, { message: 'malformed JSON body' });
    return;
  }
  const operations = Array.isArray(parsed) ? parsed : [parsed];
  const ctx: GraphContext = { store, login };
  const responses: Array<Record<string, unknown>> = [];
  for (const operation of operations) {
    if (!isRecord(operation) || typeof operation.query !== 'string') {
      responses.push({ errors: [{ message: 'a GraphQL query string is required' }] });
      continue;
    }
    let document: DocumentNode;
    try {
      document = new Parser(tokenize(operation.query)).parseDocument();
    } catch (error) {
      responses.push({ errors: [{ message: error instanceof Error ? error.message : 'failed to parse the query' }] });
      continue;
    }
    if (document.operations.length === 0) {
      responses.push({ errors: [{ message: 'no operation in the document' }] });
      continue;
    }
    const variables = isRecord(operation.variables) ? (operation.variables as Record<string, unknown>) : {};
    const named = typeof operation.operationName === 'string' ? operation.operationName : null;
    const chosen = named === null ? document.operations[0] : document.operations.find((candidate) => candidate.name === named) ?? document.operations[0];
    responses.push(executeOperation(chosen, document, variables, ctx));
  }
  sendJson(res, 200, Array.isArray(parsed) ? responses : responses[0]);
}
