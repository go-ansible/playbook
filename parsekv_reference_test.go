package playbook

// GENERATED from real ansible-core 2.21.4 by driving its own
// ansible.parsing.splitter.parse_kv over each input, once with
// check_raw false and once true. Do not hand-edit: the point of the
// table is that nobody chose these values.
var parseKVReference = []struct {
	name, in string
	kv, raw  map[string]any
	kvErr    bool
}{
	{"plain", "msg=plain", map[string]any{"msg": "plain"}, map[string]any{"_raw_params": "msg=plain"}, false},
	{"double quoted", "msg=\"kv form\"", map[string]any{"msg": "kv form"}, map[string]any{"_raw_params": "msg=\"kv form\""}, false},
	{"single quoted", "msg='single quoted'", map[string]any{"msg": "single quoted"}, map[string]any{"_raw_params": "msg='single quoted'"}, false},
	{"equals inside quotes", "msg=\"has=equals\"", map[string]any{"msg": "has=equals"}, map[string]any{"_raw_params": "msg=\"has=equals\""}, false},
	{"brackets", "msg=\"has [brackets]\"", map[string]any{"msg": "has [brackets]"}, map[string]any{"_raw_params": "msg=\"has [brackets]\""}, false},
	{"jinja with equals", "msg=\"tmpl={{ 1 + 1 }}\"", map[string]any{"msg": "tmpl={{ 1 + 1 }}"}, map[string]any{"_raw_params": "msg=\"tmpl={{ 1 + 1 }}\""}, false},
	{"two options", "path=/tmp/x state=touch", map[string]any{"path": "/tmp/x", "state": "touch"}, map[string]any{"_raw_params": "path=/tmp/x state=touch"}, false},
	{"three options", "a=1 b=2 c=3", map[string]any{"a": "1", "b": "2", "c": "3"}, map[string]any{"_raw_params": "a=1 b=2 c=3"}, false},
	{"empty value", "msg=", map[string]any{"msg": ""}, map[string]any{"_raw_params": "msg="}, false},
	{"surrounding spaces", "  msg=spaced  ", map[string]any{"_raw_params": " ", "msg": "spaced"}, map[string]any{"_raw_params": "  msg=spaced  "}, false},
	{"no pairs at all", "echo hello", map[string]any{"_raw_params": "echo hello"}, map[string]any{"_raw_params": "echo hello"}, false},
	{"raw then pair", "echo hello msg=x", map[string]any{"_raw_params": "echo hello", "msg": "x"}, map[string]any{"_raw_params": "echo hello msg=x"}, false},
	{"pair then raw", "msg=x echo hello", map[string]any{"_raw_params": "echo hello", "msg": "x"}, map[string]any{"_raw_params": "msg=x echo hello"}, false},
	{"escaped equals alone", "a\\=b", map[string]any{"_raw_params": "a=b"}, map[string]any{"_raw_params": "a=b"}, false},
	{"escaped equals in value", "k=a\\=b", map[string]any{"k": "a\\=b"}, map[string]any{"_raw_params": "k=a\\=b"}, false},
	{"two equals", "k=v=w", map[string]any{"k": "v=w"}, map[string]any{"_raw_params": "k=v=w"}, false},
	{"two quoted values", "msg=\"a b\" other=\"c d\"", map[string]any{"msg": "a b", "other": "c d"}, map[string]any{"_raw_params": "msg=\"a b\" other=\"c d\""}, false},
	{"command options", "chdir=/tmp echo hi", map[string]any{"_raw_params": "echo hi", "chdir": "/tmp"}, map[string]any{"_raw_params": "echo hi", "chdir": "/tmp"}, false},
	{"all command options", "creates=/tmp/f removes=/tmp/g echo hi", map[string]any{"_raw_params": "echo hi", "creates": "/tmp/f", "removes": "/tmp/g"}, map[string]any{"_raw_params": "echo hi", "creates": "/tmp/f", "removes": "/tmp/g"}, false},
	{"executable and warn", "executable=/bin/sh warn=no echo hi", map[string]any{"_raw_params": "echo hi", "executable": "/bin/sh", "warn": "no"}, map[string]any{"_raw_params": "echo hi", "executable": "/bin/sh", "warn": "no"}, false},
	{"src dest", "src=a dest=b", map[string]any{"dest": "b", "src": "a"}, map[string]any{"_raw_params": "src=a dest=b"}, false},
	{"apostrophe inside", "msg=\"it's here\"", map[string]any{"msg": "it's here"}, map[string]any{"_raw_params": "msg=\"it's here\""}, false},
	{"equals inside quoted value", "key=\"value with = sign\"", map[string]any{"key": "value with = sign"}, map[string]any{"_raw_params": "key=\"value with = sign\""}, false},
	{"raw between pairs", "a=1 free words b=2", map[string]any{"_raw_params": "free words", "a": "1", "b": "2"}, map[string]any{"_raw_params": "a=1 free words b=2"}, false},
	{"empty string", "", map[string]any{}, map[string]any{}, false},
	{"bare word", "ping", map[string]any{"_raw_params": "ping"}, map[string]any{"_raw_params": "ping"}, false},
	{"jinja spanning spaces", "msg={{ a + b }}", map[string]any{"msg": "{{ a + b }}"}, map[string]any{"_raw_params": "msg={{ a + b }}"}, false},
	{"newline in args", "a=1\nb=2", map[string]any{"a": "1", "b": "2"}, map[string]any{"_raw_params": "a=1\nb=2"}, false},
}
