package playbook

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEveryUntrustedStoreGoesThroughTheHelper refuses a RAW write into
// the Registered or Facts layer. Those layers hold data from outside the
// playbook, and a value stored without recording its provenance is a
// value that will be re-rendered — which is how a managed host's data
// came to be evaluated on the control node.
//
// It is written after the mistake it catches, for the second time in two
// days. Marking three obvious entry points by hand left FIVE other
// stores into vars.Registered unmarked, and the proof of concept fired
// again immediately. Storing and marking now happen in ONE statement
// (setVarUntrusted), so they cannot drift, and this refuses any new
// caller that goes around it.
//
// Two exceptions, each recognised by what it is rather than by a line
// number:
//
//   - include_vars writes an author's OWN file, and real TEMPLATES such
//     a file — marking it broke `greeting: "Hello {{ who }}"`;
//   - meta: clear_facts stores an EMPTY map, so there is nothing whose
//     provenance could matter.
//
// Each declares itself with a `provenance: trusted` comment at the store
// rather than being recognised by a nearby function name, which is how
// the first version of this check missed include_vars: the function is
// long enough that its name was out of window.
func TestEveryUntrustedStoreGoesThroughTheHelper(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	// The helper must actually be in use, or "no raw stores" would also
	// be satisfied by an engine that stores nothing at all.
	if n := strings.Count(text, "setVarUntrusted(") + strings.Count(text, "setMapUntrusted("); n < 5 {
		t.Fatalf("the helper is used %d times; either the engine stopped storing results "+
			"or this check is reading the wrong thing — a pass would mean nothing", n)
	}

	raw := regexp.MustCompile(`\.(SetVar|Set)\((?:vars\.)?(Registered|Facts)[,)]`)
	for _, loc := range raw.FindAllStringIndex(text, -1) {
		from := loc[0] - 500
		if from < 0 {
			from = 0
		}
		window := text[from : loc[1]+200]
		// An exception declares itself IN THE CODE, so the check does
		// not depend on how far away a function name happens to be --
		// the first version missed include_vars for exactly that reason.
		if strings.Contains(window, "provenance: trusted") {
			continue
		}
		t.Errorf("a RAW store into an untrusted layer at offset %d: use setVarUntrusted, "+
			"or the value will be re-rendered\n\t%s",
			loc[0], firstLineOf(text[loc[0]:loc[1]+120]))
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
