package puff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"a list's lines are 2 past its (",
			"(def (f x)\n(let [a 1]\n        (g a\n  x)))\n",
			"(def (f x)\n  (let [a 1]\n    (g a\n      x)))\n"},
		{"a vector's and a map's line up with their first element",
			"(def xs [1\n  2\n      3])\n(def m {:a 1\n:b 2})\n",
			"(def xs [1\n         2\n         3])\n(def m {:a 1\n        :b 2})\n"},
		{"closing brackets join the line before",
			"(def (f x)\n  (g x)\n)\n",
			"(def (f x)\n  (g x))\n"},
		{"but not a comment",
			"(def (f x)\n  (g x) ; why\n  )\n",
			"(def (f x)\n  (g x) ; why\n)\n"},
		{"one space between tokens, none inside brackets",
			"( f   x [ 1  2 ]   )\n",
			"(f x [1 2])\n"},
		{"a comment keeps the space before it, and its own line's indent",
			"(def every 5m)   ; how often\n(def (f)\n      ; the answer\n  42)\n",
			"(def every 5m)   ; how often\n(def (f)\n  ; the answer\n  42)\n"},
		{"a comment right after (",
			"(do; first\n  1)\n",
			"(do ; first\n  1)\n"},
		{"one blank line at most, none at the ends",
			"\n\n(def a 1)\n\n\n\n(def b 2)\n\n",
			"(def a 1)\n\n(def b 2)\n"},
		{"trailing spaces go, and the file ends in a newline",
			"(def a 1)   \n(def b 2)",
			"(def a 1)\n(def b 2)\n"},
		{"tokens stay as written",
			"(def m {:b 1.50 :a \"x\\ty\"})\n(def d 1000ms)\n",
			"(def m {:b 1.50 :a \"x\\ty\"})\n(def d 1000ms)\n"},
		{"a string over lines stays as it is",
			"(def s \"one\n  two\")\n(def t 1)\n",
			"(def s \"one\n  two\")\n(def t 1)\n"},
		{`a """ string moves in with its line`,
			"(service s\n(def p \"\"\"\n  Hi.\n    Indented.\n\n  \"\"\"))\n",
			"(service s\n  (def p \"\"\"\n    Hi.\n      Indented.\n\n    \"\"\"))\n"},
		{`and out`,
			"(service s\n      (def p \"\"\"\n        Hi.\n        \"\"\"))\n",
			"(service s\n  (def p \"\"\"\n    Hi.\n    \"\"\"))\n"},
		{"nothing", "", ""},
	}
	for _, tt := range tests {
		got, err := Format("t.puff", tt.src)
		if err != nil || got != tt.want {
			t.Errorf("%s: Format(%q) = %q, %v; want %q", tt.name, tt.src, got, err, tt.want)
			continue
		}
		if again, _ := Format("t.puff", got); again != got {
			t.Errorf("%s: formatting again = %q, want it unchanged", tt.name, again)
		}
	}
	if _, err := Format("t.puff", "(def a\n  (f 1)"); err == nil || err.Error() != "t.puff:1:1: no closing ')'" {
		t.Errorf("an unclosed form = %v", err)
	}
}

// TestFormatFiles formats every puff file in the repository: each keeps
// its code, and formatting it again changes nothing.
func TestFormatFiles(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "services", "*.puff"))
	examples, _ := filepath.Glob(filepath.Join("..", "..", "examples", "*.puff"))
	files = append(files, examples...)
	if len(files) == 0 {
		t.Fatal("no puff files")
	}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Format(file, string(src))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if again, _ := Format(file, got); again != got {
			t.Errorf("%s: formatting it twice gives something else", file)
		}
		if strings.Contains(got, " \n") {
			t.Errorf("%s: trailing spaces after formatting", file)
		}
	}
}
