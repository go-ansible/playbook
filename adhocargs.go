package playbook

import (
	"encoding/json"
	"errors"
	"strings"
)

// ParseModuleArgs turns an ad-hoc `-a` argument string into a module's
// arguments, the way real's ansible-adhoc does it.
//
// It exists because this package already carries the faithful port of
// real's parse_kv/split_args — quote state, nested {{ }} and {% %}
// blocks, escaped equals, _raw_params — and the ad-hoc path needs
// exactly that. cli/internal/adhoc had its own strings.Fields version,
// which could not see quotes or Jinja blocks and SILENTLY DROPPED any
// token without an equals sign, so `-a 'msg=hello world'` ran with
// msg="hello" and `-a 'msg={{ v }}'` ran with msg="{{".
//
// Two lists, and they are not the same list: freeformActions is real's
// MODULE_REQUIRE_ARGS (command, raw, script, shell, win_command,
// win_shell), which decides whether a k=v is an OPTION or part of the
// free-form text; rawParamModules is real's wider RAW_PARAM_MODULES,
// which decides whether a module may receive _raw_params at all.
// Reading check_raw off the wrong one would make `set_fact a=1` a raw
// param.
//
// A JSON object is tried first, as real's ad-hoc does. Where real
// differs: on a string that merely LOOKS like JSON, `-a '{not json}'`,
// real raises and exits 250 with "Unexpected Exception". That is a
// traceback, not a behaviour to match, so this falls through to the
// k=v parser — which for most modules then reports the same
// raw-params refusal real would have given for any other unparseable
// argument.
func ParseModuleArgs(module, args string) (map[string]any, error) {
	if t := strings.TrimSpace(args); strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
		var m map[string]any
		if err := json.Unmarshal([]byte(t), &m); err == nil {
			return m, nil
		}
	}
	m, err := parseKV(args, freeformActions[module])
	if err != nil {
		return nil, err
	}
	// Real's own check, in real's own words: a module that cannot take
	// free-form text and was handed some is an error, not an argument
	// quietly ignored. Measured: exit 2, "[ERROR]: Task failed: Action
	// 'ansible.builtin.debug' does not support raw params."
	if _, hasRaw := m["_raw_params"]; hasRaw && !rawParamModules[module] {
		return nil, errors.New(rawParamsRefusal(module))
	}
	return m, nil
}
