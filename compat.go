package playbook

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-ansible/modules"
	"gopkg.in/yaml.v3"
)

// CompatFinding is one construct in a playbook that no current Ansible
// will run — neither the reference nor this port — together with what to
// write instead.
//
// It exists because the reference's own diagnosis names the symptom and
// not the cure. Measured against ansible-core 2.21.4, a play carrying a
// 2015-era `sudo: yes` exits 4 with "'sudo' is not a valid attribute for
// a Play", and the same key on a TASK exits 4 with "conflicting action
// statements: debug, sudo" — which does not even hint that `sudo` was
// ever a keyword. Users reach for an old virtualenv because the error
// tells them nothing else to try.
type CompatFinding struct {
	// Line and Column are 1-based positions in the source, as yaml.v3
	// reports them. Zero means the finding could not be located — a
	// module name that only appears after a template expansion, say.
	Line   int
	Column int

	// Kind is "play keyword", "task keyword" or "module".
	Kind string

	// Name is the offending key or module name as the playbook spells it.
	Name string

	// Advice is what to write instead, in one sentence.
	Advice string
}

func (f CompatFinding) String() string {
	loc := ""
	if f.Line > 0 {
		loc = fmt.Sprintf("%d:%d: ", f.Line, f.Column)
	}
	return fmt.Sprintf("%s%s %q: %s", loc, f.Kind, f.Name, f.Advice)
}

// retiredKeywords maps a keyword no current Ansible accepts to the
// advice this port gives for it.
//
// Every entry is there because it was MEASURED to be refused, not
// because a changelog was remembered: TestRetiredKeywordsAreRefusedByReference
// runs each one through the installed reference, at play level and at
// task level, and fails if the reference accepts it. The same test
// carries its own control — `become` and `when` must be accepted — so a
// broken reference cannot turn into a table of imaginary removals.
//
// Deliberately absent: the version each one went away in. The reference
// does not ship that history, and the porting guides do not cover every
// entry; a version number here would be a claim this port cannot back.
// What it can say is what the keyword meant and what expresses it today.
var retiredKeywords = map[string]string{
	"sudo":      "use `become: true`",
	"sudo_user": "use `become_user:`",
	"su":        "use `become: true` with `become_method: su`",
	"su_user":   "use `become_user:` with `become_method: su`",

	"always_run": "use `check_mode: false` — it is the same meaning with the " +
		"polarity flipped, so `always_run: yes` becomes `check_mode: false`",

	"first_available_file": "use `with_first_found:`, or the `first_found` lookup",

	"accelerate":      "the accelerate connection was removed; use the default `ssh` connection, with pipelining for the speed it was there for",
	"accelerate_port": "the accelerate connection was removed; use the default `ssh` connection, with pipelining for the speed it was there for",

	"squash_actions": "pass the loop's list to the module's own list-taking option " +
		"(`name:` on the package modules) instead of looping",

	"include": "use `import_tasks:` for a file to pull in at parse time, or " +
		"`include_tasks:` for one chosen while the play runs",
}

// taskListKeys are the play keys whose value is a list of tasks, and so
// the places a task-level keyword can appear.
var taskListKeys = map[string]bool{
	"tasks":      true,
	"pre_tasks":  true,
	"post_tasks": true,
	"handlers":   true,
	"block":      true,
	"rescue":     true,
	"always":     true,
}

// CompatScan reports every construct in a playbook source that no
// current Ansible will run, and what to write instead.
//
// It works on sources that do NOT parse, which is the whole point: a
// playbook carrying `sudo:` fails to parse in the reference and here
// alike, so a report that needed a parsed playbook would have nothing to
// say in exactly the case it is for. Keyword findings come from a
// tolerant walk of the YAML; module findings are added only when the
// source does parse, since resolving a module name needs the play
// structure.
//
// A nil error with no findings means the scan ran and found nothing. An
// error means the source is not YAML at all.
func CompatScan(src []byte) ([]CompatFinding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("playbook: compat scan: %w", err)
	}

	s := &compatScan{seen: map[string]*yaml.Node{}}
	if len(doc.Content) > 0 {
		s.walkPlays(doc.Content[0])
	}

	// Module names need the parse. When the source does not parse — most
	// often BECAUSE of a keyword above — the keyword findings stand on
	// their own, and the user gets them before anything else.
	if pb, err := Parse(src); err == nil {
		s.addModules(pb)
	}

	sort.SliceStable(s.findings, func(i, j int) bool {
		if s.findings[i].Line != s.findings[j].Line {
			return s.findings[i].Line < s.findings[j].Line
		}
		return s.findings[i].Column < s.findings[j].Column
	})
	return s.findings, nil
}

// CompatScanFile is CompatScan on the playbook at path, decrypting it
// first when it is vault-encrypted. An empty password behaves like
// ParseFile.
//
// It exists because the CLI has a path, not bytes, and a vault-encrypted
// playbook is exactly as likely to be an old one: reading it with
// os.ReadFile would hand the scan a header line and a wall of base64,
// and the report would be "this is not YAML" for a playbook that is.
func CompatScanFile(path, vaultPassword string) ([]CompatFinding, error) {
	data, err := readMaybeEncrypted(path, vaultPassword)
	if err != nil {
		return nil, err
	}
	return CompatScan(data)
}

type compatScan struct {
	findings []CompatFinding
	// seen records where each key inside a task mapping was written, so
	// a module name resolved later can be given a line.
	seen map[string]*yaml.Node
}

func (s *compatScan) walkPlays(n *yaml.Node) {
	if n.Kind != yaml.SequenceNode {
		return
	}
	for _, play := range n.Content {
		s.walkMapping(play, "play keyword")
	}
}

// walkMapping reports retired keys in one mapping and descends into any
// value that is itself a list of tasks. It is used for plays and for
// tasks alike: the two differ only in what a finding is called, and in
// the reference's wording when it refuses one.
func (s *compatScan) walkMapping(n *yaml.Node, kind string) {
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if advice, ok := retiredKeywords[key.Value]; ok {
			s.findings = append(s.findings, CompatFinding{
				Line: key.Line, Column: key.Column,
				Kind: kind, Name: key.Value, Advice: advice,
			})
		}
		if kind == "task keyword" {
			if _, dup := s.seen[key.Value]; !dup {
				s.seen[key.Value] = key
			}
		}
		if taskListKeys[key.Value] && val.Kind == yaml.SequenceNode {
			for _, task := range val.Content {
				s.walkMapping(task, "task keyword")
			}
		}
	}
}

// addModules turns this engine's own unresolved-module check into
// findings, with advice where the name's fate can be worked out.
func (s *compatScan) addModules(pb Playbook) {
	for _, u := range (&Engine{}).UnresolvedModules(pb) {
		f := CompatFinding{Kind: "module", Name: u.Module, Advice: moduleAdvice(u.Module)}
		if n, ok := s.seen[u.Module]; ok {
			f.Line, f.Column = n.Line, n.Column
		}
		s.findings = append(s.findings, f)
	}
}

// moduleAdvice says what to do about a module name nothing here can run.
//
// One rule earns a real answer. The 2.9 porting guide records that the
// `*_facts` modules were renamed to `*_info`; where the renamed module
// IS registered here, the advice names it, and
// TestFactsRenameAdviceNamesARegisteredModule holds that promise by
// checking every such sentence against the registry.
//
// Everything else gets the honest answer: a bare module name is NOT a
// compatibility problem in this port. The split that moved modules out
// of core in 2.10 is invisible here because the registry is keyed by
// short name, so a 2.9-era `ufw:` resolves exactly as
// `community.general.ufw` does. A name that does not resolve is a name
// this port has not ported yet, which no flag can conjure.
func moduleAdvice(name string) string {
	short := modules.NormalizeName(name)
	if base, ok := strings.CutSuffix(short, "_facts"); ok {
		if _, found := modules.Default().Get(base + "_info"); found {
			return fmt.Sprintf("the `*_facts` modules were renamed to `*_info`; use `%s`", base+"_info")
		}
	}
	return "not registered in this port; this is missing coverage rather than a " +
		"version difference — bare names resolve to their collection already"
}
