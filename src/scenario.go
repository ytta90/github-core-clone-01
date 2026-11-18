package main

// The viewer login is a pure function of the contest's fixed scenario seed
// (wire.ts SCENARIO_SEED): the first user generateUsers(scenarioRng("users"), 12)
// builds. Ported from wire.ts so the forge answers viewer.login without ever
// seeing the scenario: token -> login is derived, never guessed.

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

func fnv1a64(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		h ^= uint64(text[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type rng struct{ state uint64 }

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *rng) nextInt(max int) int {
	if max <= 1 {
		return 0
	}
	const range32 = 4294967296
	limit := range32 - (range32 % uint64(max))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(max))
		}
	}
}

func (r *rng) pick(list []string) string { return list[r.nextInt(len(list))] }

func scenarioRng(label string) *rng {
	return &rng{state: mix64(scenarioSeed ^ fnv1a64(label))}
}

// viewerIdentity returns the token owner's login, display name and email.
func viewerIdentity() (login, name, email string) {
	r := scenarioRng("users")
	first := r.pick(firstNames)
	last := r.pick(lastNames)
	login = first + last
	name = first + " " + last
	email = login + "@example.invalid"
	return
}
