#!/usr/bin/env python3
"""Generates scorer_vectors.json: cases scored by the OFFICIAL GAIA scorer.

Development tool only; nothing at run time uses Python. It downloads the
official scorer.py from the GAIA leaderboard space, runs it over a table of
cases, and writes the answers it gives. The Go scorer (core/score) must agree
with every one of them. The official file itself is not stored in the repository.

    python3 internal/eval/testdata/gen_scorer_vectors.py

Run it again only to add cases or to follow a change of the official scorer;
the new vectors are reviewed like any other change.
"""
import contextlib
import hashlib
import importlib.util
import io
import json
import random
import sys
import tempfile
import types
import urllib.request
import warnings
from pathlib import Path

SCORER_URL = "https://huggingface.co/spaces/gaia-benchmark/leaderboard/raw/main/scorer.py"
OUT = Path(__file__).with_name("scorer_vectors.json")


def load_official_scorer():
    """Downloads scorer.py and imports it. numpy is imported by the file but never used."""
    with urllib.request.urlopen(SCORER_URL, timeout=30) as response:
        source = response.read()
    sys.modules.setdefault("numpy", types.ModuleType("numpy"))
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "scorer.py"
        path.write_bytes(source)
        spec = importlib.util.spec_from_file_location("official_scorer", path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module, hashlib.sha256(source).hexdigest()


# (answer, truth). An answer of None is the official scorer's "no answer".
HAND = [
    # --- numbers: units, commas and the formats float() accepts
    ("1000", "1000"), ("$1,000", "1000"), ("1,000", "1000"), ("1000.0", "1000"), ("1e3", "1000"), ("1E3", "1000"),
    (" 1000 ", "1000"), ("1_000", "1000"), ("$1,000.00", "1000"), ("1000%", "1000"), ("10 00", "1000"),
    ("one thousand", "1000"), ("", "1000"), ("None", "1000"), ("abc", "1000"), ("1000 dollars", "1000"),
    (".5", "0.5"), ("50%", "0.5"), ("0.50", "0.5"), ("1/2", "0.5"), ("0.5", ".5"), ("+0.5", "0.5"),
    ("-3", "-3"), ("−3", "-3"), ("(3)", "-3"), ("+3", "3"), ("- 3", "-3"), ("-3.0", "-3"), ("--3", "-3"),
    ("5.", "5"), ("5", "5."), ("05", "5"), ("5e0", "5"), ("5.0e0", "5"), ("0x10", "16"), ("16", "0x10"),
    ("1e400", "inf"), ("inf", "inf"), ("garbage", "inf"), ("infinity", "inf"), ("-inf", "-inf"), ("garbage", "-inf"),
    ("nan", "nan"), ("NaN", "nan"), ("1", "nan"), ("1e999", "1e999"), ("1e999", "inf"),
    ("3", "٣"), ("٣", "3"), ("３", "3"), ("3", "３"), ("١٢", "12"), ("12", "١٢"),
    ("1_0", "10"), ("1__0", "10"), ("_1", "1"), ("1_", "1"), ("1_.5", "1.5"), ("1._5", "1.5"), ("1e1_0", "1e10"),
    ("0.1", "0.1"), ("1e-1", "0.1"), ("10%", "0.1"), ("0.10", "0.1"), ("0.1 ", "0.1"), (" 0.1", "0.1"),
    ("1000000000000000000000", "1e21"), ("1e21", "1000000000000000000000"), ("0", "0"), ("-0", "0"), ("0.0", "-0"),
    ("1,000,000", "1000000"), ("$1.5M", "1500000"), ("1.5", "1.50"), ("3.14159", "3.14159"), ("3.1416", "3.14159"),
    ("5 6", "5"), ("5, 6", "5"), ("5;6", "5"), ("\t7\n", "7"), ("7 ", "7"), ("7​", "7"),
    # --- numbers: float() skips the six ASCII spaces, and not the separators U+001C to U+001F that str.isspace counts
    ("\x0b7\x0c", "7"), ("\x1f7\x1c", "7"), ("\x1d7\x1e", "7"), ("7", "\x1f7\x1c"), ("7.0", "\x1f7\x1c"), ("1, \x1f7\x1c", "1, 7"),
    # --- lists: split on , or ; in the truth
    ("a;b;c", "a, b, c"), ("A , B ,C", "a, b, c"), ("a,b", "a, b, c"), ("c, b, a", "a, b, c"), ("a, b, c, d", "a, b, c"),
    ("1;2;3", "1, 2, 3"), ("1,2,3.0", "1, 2, 3"), ("1, two, 3", "1;TWO;3"), ("1, 2, 3", "1; 2; 3"), ("1 2 3", "1, 2, 3"),
    ("1,000", "1,000"), ("1000", "1,000"), ("1, 000", "1,000"), ("1,0", "1,000"), ("$1,$000", "1,000"),
    ("x, y", "x;y"), ("x;y", "x,y"), ("x y", "x,y"), ("", "a,b"), ("None", "a,b"), (None, "a,b"),
    ("a.b, c", "ab, c"), ("A.B, C", "a.b,c"), ("ab, c", "a.b, c"), ("red, 3", "Red;3.0"), ("Red;3.00", "red, 3"),
    ("a,,b", "a,,b"), ("a, b", "a,,b"), ("a,", "a,"), ("a", "a,"), ("a,b,", "a,b"), (",a", ",a"), ("a", ",a"),
    ("3, red", "3;Red"), ("3;Red", "3, red"), ("$3, red", "3, red"), ("3%, red", "3, red"), ("3.0, RED", "3, red"),
    ("apple, banana", "Apple,Banana"), ("apple banana", "Apple,Banana"), ("apple;banana;", "Apple,Banana"),
    ("1,2", "1,2,"), ("1, 2,", "1,2,"), ("12", "1,2"), ("1 , 2", "1;2"), ("1;2", "1 , 2"),
    ("New York, Paris", "new york;paris"), ("NewYork,Paris", "new york;paris"), ("New-York,Paris", "new york;paris"),
    ("it's, fine", "its, fine"), ("its, fine", "it's, fine"), ("U.S., Canada", "us, canada"),
    ("1, 2, 3", "1, 2, 3\n"), ("1,2,3\n", "1, 2, 3"), ("a,b\n,c", "a,b,c"),
    # --- strings: whitespace, case and ASCII punctuation are ignored
    ("Saint Petersburg", "Saint Petersburg"), ("saint-petersburg.", "Saint Petersburg"), ("St. Petersburg", "Saint Petersburg"),
    ("sea gull", "seagull"), ("seagull", "sea gull"), ("SEAGULL", "sea gull"), ("sea  \t gull", "sea gull"),
    ("O'Brien", "obrien"), ("obrien", "O'Brien"), ("U.S.A.", "usa"), ("usa", "U.S.A."), ("e-mail", "email"), ("C++", "c"),
    ("100%", "100"), ("100", "100%"), ("100%", "100%"), ("!!!", ""), ("", "!!!"), ("", ""), ("   ", ""), ("a b", "ab"),
    ("None", "None"), ("none", "None"), (None, "None"), (None, "none"), (None, "5"), (None, ""), ("None", "5"),
    ("Paris", "paris"), ("Paris!", "Paris"), ("(Paris)", "Paris"), ("[Paris]", "Paris"), ("\"Paris\"", "Paris"),
    ("`Paris`", "Paris"), ("Paris_", "Paris"), ("Pa~ris", "Paris"), ("Pa|ris", "Paris"), ("Pa\\ris", "Paris"),
    ("Pa/ris", "Paris"), ("Pa:ris", "Paris"), ("Pa@ris", "Paris"), ("Pa#ris", "Paris"), ("Pa$ris", "Paris"),
    ("Pa^ris", "Paris"), ("Pa&ris", "Paris"), ("Pa*ris", "Paris"), ("Pa=ris", "Paris"), ("Pa<ris>", "Paris"),
    ("New\tYork\n", "new york"), ("New York", "new york"), ("New York", "new york"), ("New​York", "new york"),
    ("New　York", "new york"), ("New\u0085York", "new york"), ("New\x1cYork", "new york"), ("New\x1fYork", "new york"),
    ("NewYork ", "new york"), ("New᠎York", "new york"), ("New﻿York", "new york"),
    # --- Unicode: lowercasing is Python's, with its special cases
    ("Zürich", "zürich"), ("ZÜRICH", "zürich"), ("Zürich", "zürich"), ("zürich", "Zürich"),
    ("İstanbul", "i̇stanbul"), ("İstanbul", "istanbul"), ("istanbul", "İstanbul"), ("İstanbul", "İstanbul"),
    ("ΑΣ", "ας"), ("ΑΣ", "ασ"), ("ας", "ΑΣ"), ("ΑΣ Β", "ασ β"),
    ("Σ", "σ"), ("Σ", "ς"), ("ΑΣ.", "ας"), ("Α'Σ", "α'ς"), ("ΣΑ", "σα"),
    ("Straße", "STRASSE"), ("STRASSE", "Straße"), ("Straẞe", "Straße"), ("ẞ", "ß"), ("ß", "ẞ"),
    ("naïve café", "NaÏve CafÉ"), ("日本語", "日本語"), ("日 本", "日本"),
    ("\U0001F642", "\U0001F642"), ("\U0001F642 ", "\U0001F642"), ("a\U0001F642b", "ab"), ("é", "é"), ("é", "é"),
    ("“Paris”", "Paris"), ("‘Paris’", "Paris"), ("Paris—France", "ParisFrance"), ("Paris，France", "Paris, France"),
    ("Ångström", "ångström"), ("Ǆ", "ǆ"), ("ǅ", "ǆ"), ("ǆ", "Ǆ"),
    ("Владимир", "владимир"), ("مرحبا", "مرحبا"),
    ("Ⅰ", "ⅰ"), ("Ⓐ", "ⓐ"), ("Ａ", "ａ"), ("ＡＢ", "ab"),
    # --- a number as the answer to a string, and a string to a number
    ("42", "forty-two"), ("forty two", "forty-two"), ("fortytwo", "forty-two"), ("42", "4 2"), ("4 2", "42"),
    ("five", "5"), ("5", "five"), ("Five", "five"), ("1st", "first"), ("2nd", "2"),
]

TEMPLATES_NUM = ["{}", "${}", "{}%", " {} ", "{},0", "{}.0", "{}e0", "+{}", "-{}", "{}\n", "\t{}"]
WORDS = ["Paris", "new york", "St. Louis", "sea gull", "O'Neil", "Zürich", "ΑΣ", "İzmir", "Straße", "e-mail", "A.B.C.", "x_y"]


def generated(rng):
    cases = []
    for _ in range(60):
        n = rng.choice([0, 1, 7, 42, 100, 1000, 12345, 3.5, 0.25, 1e6, -8, -0.5])
        truth = str(n)
        template = rng.choice(TEMPLATES_NUM)
        shown = f"{n:,}" if isinstance(n, int) and rng.random() < 0.5 else str(n)
        cases.append((template.format(shown), truth))
    for _ in range(40):
        size = rng.randint(2, 4)
        items = [rng.choice([str(rng.randint(0, 99)), rng.choice(WORDS), str(rng.random())[:5]]) for _ in range(size)]
        sep_truth = rng.choice([",", ";", ", ", "; "])
        sep_answer = rng.choice([",", ";", ", ", "; ", " , "])
        answer_items = [i.upper() if rng.random() < 0.3 else i for i in items]
        if rng.random() < 0.25:
            answer_items = answer_items[:-1]
        cases.append((sep_answer.join(answer_items), sep_truth.join(items)))
    for _ in range(40):
        word = rng.choice(WORDS)
        mangled = rng.choice([word.lower(), word.upper(), word.replace(" ", ""), word + ".", "(" + word + ")", word.replace(" ", "-")])
        cases.append((mangled, word))
    return cases


def main():
    scorer, digest = load_official_scorer()
    cases, seen = [], set()
    for answer, truth in HAND + generated(random.Random(20261002)):
        key = (answer, truth)
        if key in seen:
            continue
        seen.add(key)
        with contextlib.redirect_stdout(io.StringIO()), warnings.catch_warnings():
            warnings.simplefilter("ignore")
            want = bool(scorer.question_scorer(answer, truth))
        entry = {"truth": truth, "want": want}
        if answer is None:
            entry["answer_is_none"] = True
        else:
            entry["answer"] = answer
        cases.append(entry)

    OUT.write_text(json.dumps({
        "source": SCORER_URL,
        "source_sha256": digest,
        "python": sys.version.split()[0],
        "cases": cases,
    }, ensure_ascii=True, indent=1) + "\n", encoding="utf-8")
    trues = sum(c["want"] for c in cases)
    print(f"wrote {len(cases)} cases ({trues} true, {len(cases) - trues} false) to {OUT}")


if __name__ == "__main__":
    main()
