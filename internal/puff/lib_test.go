package puff

import (
	"strings"
	"testing"
)

func testLib(t *testing.T, tests []struct{ src, want string }) {
	t.Helper()
	for _, tt := range tests {
		if got := load(NewEnv(), tt.src); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

func TestStringLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`(string/length "héllo")`, "5"},
		{`(string/concat "a" "b" "c")`, `"abc"`},
		{`(string/split "a,b,,c" ",")`, `["a" "b" "" "c"]`},
		{`(string/join ["a" "b"] ", ")`, `"a, b"`},
		{`(string/lines "a\r\nb\n")`, `["a" "b"]`},
		{`(string/lines "")`, `[]`},
		{`(string/trim "  x \n")`, `"x"`},
		{`[(string/upper "abc") (string/lower "ABC")]`, `["ABC" "abc"]`},
		{`[(string/contains? "abc" "b") (string/starts-with? "abc" "ab") (string/ends-with? "abc" "x")]`, `[#t #t #f]`},
		{`(string/replace "a-b-c" "-" "+")`, `"a+b+c"`},
		{`[(string/slice "héllo" 1 3) (string/slice "héllo" 3)]`, `["él" "lo"]`},
		{`[(string/index "héllo" "l") (string/index "abc" "x")]`, `[(some 2) (none)]`},

		{`(string/slice "abc" 2 5)`, "1:1: string/slice: 2 to 5 is out of range for length 3"},
		{`(string/join ["a" 1] ",")`, "1:1: string/join: want a string, got 1"},
		{`(string/length 1)`, "1:1: string/length: want a string, got 1"},
	})
}

func TestNumberLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`[(int/from-string "42") (int/from-string "x")]`, `[(ok 42) (err {:kind :parse :message "not an int: \"x\""})]`},
		{`(int/to-string -5)`, `"-5"`},
		{`[(int/from-float 2.9) (int/from-float -2.9) (int/from-float 1e30)]`, `[(ok 2) (ok -2) (err {:kind :range :message "1e+30 is out of range for an int"})]`},
		{`[(int/div 7 2) (int/rem 7 2) (int/div -7 2)]`, `[3 1 -3]`},
		{`[(float/from-string "2.5") (float/from-string "inf")]`, `[(ok 2.5) (err {:kind :parse :message "not a float: \"inf\""})]`},
		{`[(float/to-string 2.0) (float/from-int 3) (float/div 1.0 4.0)]`, `["2.0" 3.0 0.25]`},
		{`[(float/round 2.5) (float/floor -1.5) (float/ceil 1.2)]`, `[3.0 -2.0 2.0]`},

		{`(int/div 1 0)`, "1:1: int/div: division by zero"},
		{`(int/div -9223372036854775808 -1)`, "1:1: int/div: int overflow"},
		{`(float/div 1.0 0.0)`, "1:1: float/div: division by zero"},
	})
}

func TestValueLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`(value/to-string {:a [1 "x"]})`, `"{:a [1 \"x\"]}"`},
		{`[(value/from-string "(ok [1])") (value/from-string "(")]`, `[(ok (ok [1])) (err {:kind :parse :message "1:1: no closing ')'"})]`},
		{`[(value/type 1) (value/type "s") (value/type +) (value/type (ok 1))]`, `[:int :string :function :tag]`},
	})
}

func TestVectorLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`[(vector/length [1 2]) (vector/empty? [])]`, `[2 #t]`},
		{`[(vector/get [1 2] 1) (vector/get [1 2] 2) (vector/get [1] -1)]`, `[(some 2) (none) (none)]`},
		{`[(vector/first []) (vector/last [1 2])]`, `[(none) (some 2)]`},
		{`(vector/append [1] 2 3)`, `[1 2 3]`},
		{`(vector/concat [1] [] [2 3])`, `[1 2 3]`},
		{`(vector/slice [1 2 3] 1)`, `[2 3]`},
		{`(vector/reverse [1 2 3])`, `[3 2 1]`},
		{`(vector/contains? [1 [2]] [2])`, `#t`},
		{`[(vector/range 0 3) (vector/range 3 0)]`, `[[0 1 2] []]`},
		{`(vector/sort ["b" "c" "a"])`, `["a" "b" "c"]`},
		{`(vector/map [1 2] (fn (x) (* x 10)))`, `[10 20]`},
		{`(vector/filter [1 2 3 4] (fn (x) (> x 2)))`, `[3 4]`},
		{`(vector/reduce [1 2 3] 0 +)`, `6`},
		{`[(vector/find [1 2 3] (fn (x) (> x 1))) (vector/find [] (fn (x) #t))]`, `[(some 2) (none)]`},
		{`(vector/each [1 2] (fn (x) (ok x)))`, `(ok :done)`},
		{`(vector/each [1 2 3] (fn (x) (if (= x 2) (err :two) (ok x))))`, `(err :two)`},

		{`(vector/filter [1] (fn (x) x))`, "1:1: vector/filter: want a bool, got 1"},
		{`(vector/map [1] (fn (x) (+ x 1.0)))`, "1:25: +: can't add int and float"},
		{`(vector/map [1] 2)`, "1:1: not a function: 2"},
		{`(vector/range 0 100000000)`, "1:1: vector/range: more than 16777216 ints"},
	})
	if got := load(NewEnv(), `(vector/sort [1 "a"])`); !strings.HasPrefix(got, "1:1: vector/sort: can't compare") {
		t.Errorf("sorting mixed types: %s", got)
	}
}

func TestMapLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`[(map/get {:a 1} :a) (map/get {:a 1} :b)]`, `[(some 1) (none)]`},
		{`(map/get-or {:a 1} :b 0)`, `0`},
		{`(map/has? {"a" 1} "a")`, `#t`},
		{`(map/set {:b 1} :a 2)`, `{:a 2 :b 1}`},
		{`[(map/delete {:a 1 :b 2} :a) (map/delete {:a 1} :x)]`, `[{:b 2} {:a 1}]`},
		{`(map/merge {:a 1 :b 1} {:b 2} {:c 3})`, `{:a 1 :b 2 :c 3}`},
		{`[(map/keys {:b 1 :a 2}) (map/values {:b 1 :a 2})]`, `[[:a :b] [2 1]]`},
		{`(map/entries {:a 1})`, `[[:a 1]]`},
		{`(map/from-entries [[:b 1] [:a 2] [:b 3]])`, `{:a 2 :b 3}`},
		{`[(map/length {:a 1}) (map/empty? {})]`, `[1 #t]`},

		{`(map/set {} 1.5 1)`, "1:1: map/set: map key 1.5: must be a bool, int, string or keyword"},
		{`(map/from-entries [[:a]])`, "1:1: map/from-entries: want a [key value] pair, got [:a]"},
	})
}

func TestRegexLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{`(regex/match? "abc123" "[0-9]+")`, `#t`},
		{`[(regex/find "a1b22" "[0-9]+") (regex/find "ab" "[0-9]")]`, `[(some "1") (none)]`},
		{`(regex/find-all "a1b22" "[0-9]+")`, `["1" "22"]`},
		{`(regex/groups "key=val" "(\\w+)=(\\w+)")`, `(some ["key" "val"])`},
		{`[(regex/replace "a1b2" "[0-9]" "#") (regex/replace "k=v" "(\\w)=(\\w)" "$2=$1")]`, `["a#b#" "v=k"]`},
		{`(regex/split "a, b,c" ",\\s*")`, `["a" "b" "c"]`},

		{`(regex/match? "a" "(")`, "1:1: regex/match?: bad regex: error parsing regexp: missing closing ): `(`"},
	})
}

func TestJSONLib(t *testing.T) {
	testLib(t, []struct{ src, want string }{
		{
			`(json/parse "{\"name\": \"ann\", \"n\": 9007199254740993, \"x\": 1.5, \"ok\": true, \"tags\": [\"a\"], \"nothing\": null, \"two words\": 1}")`,
			`(ok {"two words" 1 :n 9007199254740993 :name "ann" :nothing (null) :ok #t :tags ["a"] :x 1.5})`,
		},
		{`(json/parse "[1, 2")`, `(err {:kind :parse :message "unexpected EOF"})`},
		{`(json/parse "1 2")`, `(err {:kind :parse :message "extra data after the value"})`},
		{`(json/parse "1" {})`, `(ok 1)`},
		{`(json/to-string {:a [1 2.5 "x" #t (json/null)] "b c" :kw})`, `"{\"a\":[1,2.5,\"x\",true,null],\"b c\":\"kw\"}"`},
		{`(json/to-string [1] {:indent 2})`, `"[\n  1\n]"`},
		{`(json/parse (json/to-string {:a 1}))`, `(ok {:a 1})`},
		{`(json/null)`, `(null)`},

		{`(json/to-string [1s])`, "1:1: json/to-string: can't encode duration 1s as JSON"},
		{`(json/to-string {:a 1 "a" 2})`, "1:1: json/to-string: key a appears twice"},
		{`(json/to-string [1] {:indnt 2})`, "1:1: json/to-string: unknown option :indnt"},
	})
}
