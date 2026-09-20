import json, os, random, sys
os.environ['ANSIBLE_NOCOLOR'] = '1'
from ansible.plugins.callback import CallbackBase
from ansible.utils.color import stringc

cb = CallbackBase()
print("stringc no-op check:", repr(stringc("x\n", "green")), file=sys.stderr)

def golden(before, after, bh=None, ah=None, **extra):
    d = dict(before=before, after=after, **extra)
    if bh is not None: d['before_header'] = bh
    if ah is not None: d['after_header'] = ah
    out = cb._get_diff(dict(d))
    rec = dict(before=before, after=after,
               before_header=bh or "", after_header=ah or "", expected=out)
    rec.update({k: v for k, v in extra.items()})
    return rec

cases = []
# The two shapes measured from a real --diff --check run.
cases.append(golden("", "one\ntwo\n", None, "/tmp/out/new.txt (content)"))
cases.append(golden("alpha\nbeta\n", "alpha\nbeta\ngamma\n",
                    "/tmp/seed.txt (content)", "/tmp/seed.txt (content)"))
# Hand-picked edge shapes.
for b, a in [
    ("same\n", "same\n"),
    ("gone\n", ""),
    ("a\nb\nc\n", "a\nB\nc\n"),
    ("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n",
     "1\nX\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\nY\n16\n"),
    ("1\n2\n3\n4\n5\n6\n7\n8\n", "1\nX\n3\n4\n5\n6\nY\n8\n"),
    ("a\nb", "a\nb\nc"),
    ("a\nb\n", "a\nb"),
    ("", ""),
    ("only\n", "only\nmore\n"),
    ("x\n" * 10, ""),
    ("", "y\n" * 10),
]:
    cases.append(golden(b, a, "/f.txt (content)", "/f.txt (content)"))

# Randomised pairs, to catch hunk-grouping boundaries no hand case hits.
random.seed(20260920)
alphabet = ["alpha", "beta", "gamma", "delta", "eps", "zeta", "eta"]
for _ in range(300):
    n = random.randint(0, 25)
    before = [random.choice(alphabet) for _ in range(n)]
    after = list(before)
    for _ in range(random.randint(1, 5)):
        if not after or random.random() < 0.4:
            after.insert(random.randint(0, len(after)), random.choice(alphabet))
        elif random.random() < 0.5:
            del after[random.randrange(len(after))]
        else:
            after[random.randrange(len(after))] = random.choice(alphabet)
    b = "".join(l + "\n" for l in before)
    a = "".join(l + "\n" for l in after)
    cases.append(golden(b, a, "/f.txt (content)", "/f.txt (content)"))

# Large pairs, to exercise difflib's autojunk: at 200 elements or more,
# an element occurring in more than 1% of positions is dropped from the
# match index, which changes the diff. A config file full of repeated
# lines (closing braces, blank lines) crosses that threshold routinely.
for trial in range(60):
    n = random.randint(190, 420)
    filler = ["}", "", "  option = 1"]
    before = []
    for i in range(n):
        before.append(random.choice(filler) if random.random() < 0.55
                      else "key%d = %d" % (i, random.randrange(50)))
    after = list(before)
    for _ in range(random.randint(1, 12)):
        r = random.random()
        if r < 0.35:
            after.insert(random.randint(0, len(after)), random.choice(filler))
        elif r < 0.7:
            del after[random.randrange(len(after))]
        else:
            after[random.randrange(len(after))] = "changed%d" % random.randrange(99)
    b = "".join(l + "\n" for l in before)
    a = "".join(l + "\n" for l in after)
    cases.append(golden(b, a, "/etc/app.conf (content)", "/etc/app.conf (content)"))

json.dump(cases, open("diff_cases.json", "w"), indent=1)
print("cases:", len(cases), "non-empty expected:",
      sum(1 for c in cases if c["expected"]), file=sys.stderr)
