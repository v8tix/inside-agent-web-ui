package redact_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/redact"
)

// canary is in every value that must not be shown.
const canary = "CANARY"

// printed is v as each handler of log/slog writes it under the key v: the
// attributes of the text line, and the JSON of the member. The record has no
// time, so the handlers write none. Both must have written the same keys.
func printed(t *testing.T, v slog.Value) (text, member string) {
	t.Helper()
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0)
	record.AddAttrs(slog.Attr{Key: "v", Value: v})
	var asText, asJSON bytes.Buffer
	require.NoError(t, slog.NewTextHandler(&asText, nil).Handle(t.Context(), record))
	require.NoError(t, slog.NewJSONHandler(&asJSON, nil).Handle(t.Context(), record))

	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(asText.String()), "level=INFO msg=m"))
	var line struct {
		V jsontext.Value `json:"v"`
	}
	require.NoError(t, json.Unmarshal(asJSON.Bytes(), &line), asJSON.String())
	member = string(line.V)

	assert.Equal(t, slices.Sorted(maps.Keys(pairs(t, text))), slices.Sorted(maps.Keys(flat(t, member))),
		"the text handler and the JSON handler print the same keys")
	return text, member
}

// hidden is printed, for a value that holds canaries: none is in what either
// handler wrote, nor in the text of the value itself.
func hidden(t *testing.T, v slog.Value) (text, member string) {
	t.Helper()
	text, member = printed(t, v)
	for _, shown := range []string{text, member, v.String()} {
		assert.NotContains(t, shown, canary)
	}
	return text, member
}

// pairs reads the attributes of a line of the text handler: key=value, each of
// the two in quotes when it needs them.
func pairs(t *testing.T, line string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line != "" {
		key, rest := word(t, line, "=")
		value, rest := word(t, rest, " ")
		out[key], line = value, rest
	}
	return out
}

// word is the text at the start of s up to end, without the quotes it may be
// in, and what follows end.
func word(t *testing.T, s, end string) (text, rest string) {
	t.Helper()
	quoted, err := strconv.QuotedPrefix(s)
	if err != nil {
		text, rest, _ = strings.Cut(s, end)
		return text, rest
	}
	text, err = strconv.Unquote(quoted)
	require.NoError(t, err)
	return text, strings.TrimPrefix(s[len(quoted):], end)
}

// flat reads the member of a line of the JSON handler into the keys the text
// handler writes: the names on the way to a value, joined by dots.
func flat(t *testing.T, member string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if member == "" {
		return out
	}
	var tree any
	require.NoError(t, json.Unmarshal([]byte(member), &tree), member)
	var walk func(key string, node any)
	walk = func(key string, node any) {
		object, ok := node.(map[string]any)
		if !ok {
			out[key] = fmt.Sprint(node)
			return
		}
		for name, inner := range object {
			walk(key+"."+name, inner)
		}
	}
	walk("v", tree)
	return out
}

func TestStruct_EveryHandlerPrintsTheSameFields(t *testing.T) {
	type limits struct {
		MaxConns int
		Ratio    float64
	}
	type Region struct{ Name string }
	type settings struct {
		Region
		Env    string
		Port   int
		Size   uint16
		Debug  bool
		Limits limits
		Spare  *limits
		Unset  *limits
		Notify func()
		Done   chan struct{}
	}

	v, err := redact.Struct(settings{
		Name: "eu", Env: "dev", Port: 8300, Size: 512, Debug: true,
		Limits: limits{MaxConns: 10, Ratio: 0.5}, Spare: &limits{MaxConns: 2, Ratio: 1.5},
		Notify: func() {}, Done: make(chan struct{}),
	})

	require.NoError(t, err)
	assert.Equal(t, slog.KindGroup, v.Kind())
	text, member := printed(t, v)
	assert.Equal(t, "v.Region.Name=eu v.Env=dev v.Port=8300 v.Size=512 v.Debug=true v.Limits.MaxConns=10 v.Limits.Ratio=0.5 "+
		"v.Spare.MaxConns=2 v.Spare.Ratio=1.5 v.Unset=<nil> v.Notify=[func] v.Done=[chan]", text,
		"one attribute for each field, in their order: an embedded struct under the name of its type, "+
			"and what has no value to show under the name of its kind")
	assert.Equal(t, `{"Region":{"Name":"eu"},"Env":"dev","Port":8300,"Size":512,"Debug":true,"Limits":{"MaxConns":10,"Ratio":0.5},`+
		`"Spare":{"MaxConns":2,"Ratio":1.5},"Unset":null,"Notify":"[func]","Done":"[chan]"}`, member)
	assert.Equal(t, pairs(t, text), flat(t, member), "and the values are the same too")

	// JSON has no number for these, and the JSON handler writes an error in the
	// place of a value that it cannot encode.
	t.Run("a float that JSON has no number for is its text", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Most, Least, None float64
			Rates             []float64
		}{math.Inf(1), math.Inf(-1), math.NaN(), []float64{1.5, math.Inf(1)}})

		require.NoError(t, err)
		text, member := printed(t, v)
		assert.Equal(t, `v.Most=+Inf v.Least=-Inf v.None=NaN v.Rates="[1.5 +Inf]"`, text)
		assert.Equal(t, `{"Most":"+Inf","Least":"-Inf","None":"NaN","Rates":[1.5,"+Inf"]}`, member)
	})

	t.Run("and so is a time whose year has not four digits", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Last, Never, Once time.Time
			Until             forever
		}{
			Last:  time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
			Never: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
			Once:  time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		})

		require.NoError(t, err)
		text, member := printed(t, v)
		assert.Equal(t, `v.Last=9999-12-31T23:59:59.000Z v.Never="10000-01-01 00:00:00 +0000 UTC" `+
			`v.Once="-0001-01-01 00:00:00 +0000 UTC" v.Until="10000-01-01 00:00:00 +0000 UTC"`, text)
		assert.Equal(t, `{"Last":"9999-12-31T23:59:59Z","Never":"10000-01-01 00:00:00 +0000 UTC",`+
			`"Once":"-0001-01-01 00:00:00 +0000 UTC","Until":"10000-01-01 00:00:00 +0000 UTC"}`, member)
	})
}

// forever says that it is logged as a time, one that JSON cannot write.
type forever struct{}

func (forever) LogValue() slog.Value {
	return slog.TimeValue(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
}

// counting says how it is logged, and counts how often it was asked.
type counting struct{ asked *int }

func (c counting) LogValue() slog.Value {
	*c.asked++
	return slog.StringValue("asked")
}

// A name hides the field of that name wherever one sits in the value.
func TestStruct_ANamedFieldIsHidden(t *testing.T) {
	type login struct {
		User     string
		Password string
	}
	type hosts struct {
		Password string
		Main     login
		Spare    *login
		Each     []login
		Pair     [2]login
		ByName   map[string]login
		Held     any
	}
	of := func(user string) login { return login{User: user, Password: canary + "-" + user} }

	v, err := redact.Struct(hosts{
		Password: canary + "-top", Main: of("main"), Spare: new(of("spare")),
		Each: []login{of("first"), of("second")}, Pair: [2]login{of("left"), of("right")},
		ByName: map[string]login{"b": of("b"), "a": of("a")}, Held: of("held"),
	}, "Password")

	require.NoError(t, err)
	text, _ := hidden(t, v)
	assert.Equal(t, "v.Password=[redacted] v.Main.User=main v.Main.Password=[redacted] "+
		"v.Spare.User=spare v.Spare.Password=[redacted] "+
		"v.Each.0.User=first v.Each.0.Password=[redacted] v.Each.1.User=second v.Each.1.Password=[redacted] "+
		"v.Pair.0.User=left v.Pair.0.Password=[redacted] v.Pair.1.User=right v.Pair.1.Password=[redacted] "+
		"v.ByName.a.User=a v.ByName.a.Password=[redacted] v.ByName.b.User=b v.ByName.b.Password=[redacted] "+
		"v.Held.User=held v.Held.Password=[redacted]", text)

	t.Run("several names", func(t *testing.T) {
		v, err := redact.Struct(struct{ User, Password, Token string }{"ada", canary, canary}, "Password", "Token")

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Equal(t, "v.User=ada v.Password=[redacted] v.Token=[redacted]", text)
	})

	t.Run("and the value of a named field is not asked how it is logged", func(t *testing.T) {
		asked := 0

		v, err := redact.Struct(struct{ Named, Free counting }{counting{&asked}, counting{&asked}}, "Named")

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Equal(t, "v.Named=[redacted] v.Free=asked", text)
		assert.Equal(t, 1, asked)
	})
}

// sealed says how it is logged and nothing else: it has no String and no
// MarshalText that could hide it.
type sealed string

func (sealed) LogValue() slog.Value { return slog.StringValue(redact.Placeholder) }

// vault holds an S in every place a value can sit.
type vault[S ~string] struct {
	Key    S
	Ptr    *S
	Each   []S
	Pair   [2]S
	ByName map[string]S
	Inner  struct{ Token S }
	Far    *struct{ Token S }
	Held   any
	HeldIn any
}

func vaultOf[S ~string]() vault[S] {
	return vault[S]{
		Key: canary + "-key", Ptr: new(S(canary + "-pointer")),
		Each: []S{canary + "-first", canary + "-second"}, Pair: [2]S{canary + "-left", canary + "-right"},
		ByName: map[string]S{"a": canary + "-a", "b": canary + "-b"},
		Inner:  struct{ Token S }{canary + "-inner"}, Far: &struct{ Token S }{canary + "-far"},
		Held: S(canary + "-held"), HeldIn: struct{ Token S }{canary + "-held-in"},
	}
}

// A Secret is hidden because it says how it is logged, and the view asks every
// value that does: nobody has to name its field.
func TestStruct_ASecretIsAlwaysHidden(t *testing.T) {
	tests := []struct {
		name string
		view func() (slog.Value, error)
	}{
		{"a Secret", func() (slog.Value, error) { return redact.Struct(vaultOf[envcfg.Secret]()) }},
		{"any type that says how it is logged", func() (slog.Value, error) { return redact.Struct(vaultOf[sealed]()) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.view()

			require.NoError(t, err)
			text, _ := hidden(t, v)
			assert.Equal(t, "v.Key=[redacted] v.Ptr=[redacted] v.Each.0=[redacted] v.Each.1=[redacted] "+
				"v.Pair.0=[redacted] v.Pair.1=[redacted] v.ByName.a=[redacted] v.ByName.b=[redacted] "+
				"v.Inner.Token=[redacted] v.Far.Token=[redacted] v.Held=[redacted] v.HeldIn.Token=[redacted]", text)
		})
	}
}

// What a type keeps to itself stays out of the view, whatever it is. Nothing can
// ask such a field how it is logged, and of the two handlers the text one would
// print it and the JSON one would not.
func TestStruct_FieldsThatAreNotExportedAreLeftOut(t *testing.T) {
	type base struct{ Region string }
	type kept struct {
		base
		Name  string
		note  string
		token envcfg.Secret
		inner struct{ Password string }
	}
	given := kept{Region: canary + "-region", Name: "shown", note: canary + "-note", token: canary + "-token"}
	given.inner.Password = canary + "-inner"

	v, err := redact.Struct(given)

	require.NoError(t, err)
	text, member := hidden(t, v)
	assert.Equal(t, "v.Name=shown", text, "not a plain string, not a Secret, not a struct, not an embedded one whose type has no exported name")
	assert.Equal(t, `{"Name":"shown"}`, member)

	t.Run("and none can be named, since naming one would hide nothing", func(t *testing.T) {
		for _, name := range []string{"note", "token", "base", "inner"} {
			v, err := redact.Struct(given, name)

			require.ErrorContains(t, err, `"`+name+`"`)
			text, _ := hidden(t, v)
			assert.Equal(t, "v=[redacted]", text)
		}
	})

	// A URL keeps its user and its password in fields that are not exported.
	t.Run("so the credentials inside a URL stay out", func(t *testing.T) {
		link, err := url.Parse("https://svc:" + canary + "@agent.example.com/api")
		require.NoError(t, err)

		v, err := redact.Struct(struct{ Link *url.URL }{link})

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Contains(t, text, "v.Link.Scheme=https")
		assert.Contains(t, text, "v.Link.Host=agent.example.com")
		assert.NotContains(t, text, "v.Link.User", "a struct with nothing to show is left out too")
	})
}

func TestStruct_Pointers(t *testing.T) {
	type login struct {
		User     string
		Password string
	}
	type host struct {
		Main  *login
		None  *login
		Port  *int
		Twice **login
	}
	given := host{
		Main: &login{User: "main", Password: canary}, Port: new(8300),
		Twice: new(&login{User: "far", Password: canary}),
	}
	const want = "v.Main.User=main v.Main.Password=[redacted] v.None=<nil> v.Port=8300 v.Twice.User=far v.Twice.Password=[redacted]"

	t.Run("a pointer shows what it points at, and nil when it points at nothing", func(t *testing.T) {
		v, err := redact.Struct(given, "Password")

		require.NoError(t, err)
		text, member := hidden(t, v)
		assert.Equal(t, want, text)
		assert.Contains(t, member, `"None":null`)
	})

	t.Run("the value itself may be a pointer, or a pointer to one", func(t *testing.T) {
		for _, pointer := range []any{&given, new(&given)} {
			v, err := redact.Struct(pointer, "Password")

			require.NoError(t, err)
			text, _ := hidden(t, v)
			assert.Equal(t, want, text)
		}
	})

	t.Run("a nil pointer to a struct is nil, and its names are still checked", func(t *testing.T) {
		v, err := redact.Struct((*host)(nil), "Password")

		require.NoError(t, err)
		text, member := printed(t, v)
		assert.Equal(t, "v=<nil>", text)
		assert.Equal(t, "null", member)

		_, err = redact.Struct((*host)(nil), "Pasword")
		require.Error(t, err)
	})
}

// A name that is no field would hide nothing, and the caller would not know: a
// typo in a name must not be what shows a secret. So nothing of the struct is
// shown at all.
func TestStruct_ANameThatIsNoFieldIsAnError(t *testing.T) {
	type login struct {
		User     string
		Password string
	}
	type host struct {
		Main login
		Held any
	}
	given := host{Main: login{User: "main", Password: canary + "-main"}, Held: login{User: "held", Password: canary + "-held"}}

	tests := []struct {
		name string
		v    any
		hide []string
		want []string // in the error
	}{
		{"a typo", given, []string{"Pasword"}, []string{`"Pasword"`, "redact_test.host"}},
		{"a typo next to a name that is one", given, []string{"Password", "Pasword"}, []string{`"Pasword"`}},
		{"every name that is none", given, []string{"Pasword", "Tokn"}, []string{`"Pasword"`, `"Tokn"`}},
		{"a path, which no field is called", given, []string{"Main.Password"}, []string{`"Main.Password"`}},
		{"an empty name", given, []string{""}, []string{`""`}},
		{"a name that only a value behind an interface has", struct{ Held any }{given.Main}, []string{"Password"}, []string{`"Password"`}},
		// A URL keeps a password and a time keeps a loc, both to themselves.
		{
			"a typo that is the name of a field some type keeps to itself",
			struct {
				Link     *url.URL
				Since    time.Time
				Password string
			}{Password: canary},
			[]string{"password"}, []string{`"password"`},
		},
		{
			"the same, for a time", struct {
				Since time.Time
				Loc   string
			}{Loc: canary}, []string{"loc"}, []string{`"loc"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := redact.Struct(tc.v, tc.hide...)

			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
			assert.NotContains(t, err.Error(), canary)
			text, member := hidden(t, v)
			assert.Equal(t, "v=[redacted]", text, "nothing of the struct is shown")
			assert.Equal(t, `"[redacted]"`, member)
		})
	}

	// The names are checked against the types, so a name is known also when the
	// value that would hold its field is nil or empty.
	t.Run("a field is known wherever the types can lead to it", func(t *testing.T) {
		for _, empty := range []any{
			struct{ Spare *login }{}, struct{ Each []login }{}, struct{ Pair [1]login }{},
			struct{ ByName map[string]login }{}, struct{ ByLogin map[login]string }{}, struct{ Deep []map[string]*login }{},
		} {
			_, err := redact.Struct(empty, "Password")

			assert.NoError(t, err, "%T", empty)
		}
	})
}

// loop is a pointer to a loop, which no struct is ever behind.
type loop *loop

func TestStruct_WhatIsNotAStruct(t *testing.T) {
	tests := []struct {
		name string
		v    any
	}{
		{"nil", nil},
		{"a number", 42},
		{"a string", canary},
		{"a map", map[string]string{"key": canary}},
		{"a slice of structs", []struct{ Name string }{{canary}}},
		{"a pointer to a string", new(canary)},
		{"a nil pointer to a number", (*int)(nil)},
		{"a function", func() string { return canary }},
		{"a pointer of a type that points at itself", loop(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := redact.Struct(tc.v)

			require.ErrorContains(t, err, "not a struct")
			assert.NotContains(t, err.Error(), canary)
			text, _ := hidden(t, v)
			assert.Equal(t, "v=[redacted]", text, "nothing of it is shown")
		})
	}
}

func TestStruct_ListsAndMaps(t *testing.T) {
	type user struct{ Name string }
	type lists struct {
		Tags    []string
		Ports   [2]int
		Flags   []bool
		Ratios  []float64
		Empty   []string
		NoList  []string
		Blob    []byte
		Digest  [4]byte
		Users   []user
		Grid    [][]int
		Counts  map[string]int
		ByID    map[int]user
		NoMap   map[string]int
		NoUsers []user
	}

	v, err := redact.Struct(lists{
		Tags: []string{"x", "y"}, Ports: [2]int{80, 443}, Flags: []bool{true, false}, Ratios: []float64{0.5, 2},
		Empty: []string{}, Blob: []byte(canary), Digest: [4]byte{1, 2, 3, 4},
		Users: []user{{"ada"}, {"bob"}}, Grid: [][]int{{1, 2}, {3}},
		Counts: map[string]int{"f": 6, "b": 2, "d": 4, "a": 1, "e": 5, "c": 3},
		ByID:   map[int]user{9: {"nine"}, 10: {"ten"}},
	})

	require.NoError(t, err)
	text, member := hidden(t, v)
	assert.Equal(t, `v.Tags="[x y]" v.Ports="[80 443]" v.Flags="[true false]" v.Ratios="[0.5 2]" v.Empty=[] v.NoList=<nil> `+
		`v.Blob="[6 bytes]" v.Digest="[4 bytes]" v.Users.0.Name=ada v.Users.1.Name=bob v.Grid.0="[1 2]" v.Grid.1=[3] `+
		`v.Counts.a=1 v.Counts.b=2 v.Counts.c=3 v.Counts.d=4 v.Counts.e=5 v.Counts.f=6 `+
		`v.ByID.10.Name=ten v.ByID.9.Name=nine v.NoMap=<nil> v.NoUsers=<nil>`, text,
		"plain values as the list they are, bytes as their number, the rest by index or by key, the keys sorted as text")
	assert.Equal(t, `{"Tags":["x","y"],"Ports":[80,443],"Flags":[true,false],"Ratios":[0.5,2],"Empty":[],"NoList":null,`+
		`"Blob":"[6 bytes]","Digest":"[4 bytes]","Users":{"0":{"Name":"ada"},"1":{"Name":"bob"}},"Grid":{"0":[1,2],"1":[3]},`+
		`"Counts":{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6},`+
		`"ByID":{"10":{"Name":"ten"},"9":{"Name":"nine"}},"NoMap":null,"NoUsers":null}`, member)

	// Two keys can have one text, as two secrets do. A handler writes every name
	// it is given, and a reader of JSON may refuse a line that has one twice, as
	// the one of these tests does.
	t.Run("keys with one text get a name each", func(t *testing.T) {
		v, err := redact.Struct(struct {
			BySecret map[envcfg.Secret]string
			ByAny    map[any]string
			Taken    map[string]string
		}{
			map[envcfg.Secret]string{canary + "-1": "a", canary + "-2": "b", canary + "-3": "c"},
			map[any]string{1: "a number", "1": "a text"},
			map[string]string{"k#2": "its own"},
		})

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Equal(t, `v.BySecret.[redacted]=a v.BySecret.[redacted]#2=b v.BySecret.[redacted]#3=c `+
			`v.ByAny.1="a number" v.ByAny.1#2="a text" v.Taken.k#2="its own"`, text)
	})

	// A map hands its entries out in an order of its own, which is not the same
	// from one map to the next.
	t.Run("and the same name each time, whatever order the map hands them out in", func(t *testing.T) {
		for round := range 50 {
			bySecret := map[envcfg.Secret]string{}
			for i := range 6 {
				n := (i + round) % 6
				bySecret[envcfg.Secret(canary+strconv.Itoa(n))] = strconv.Itoa(n)
			}

			v, err := redact.Struct(struct{ BySecret map[envcfg.Secret]string }{bySecret})

			require.NoError(t, err)
			text, _ := hidden(t, v)
			require.Equal(t, "v.BySecret.[redacted]=0 v.BySecret.[redacted]#2=1 v.BySecret.[redacted]#3=2 "+
				"v.BySecret.[redacted]#4=3 v.BySecret.[redacted]#5=4 v.BySecret.[redacted]#6=5", text)
		}
	})

	// The JSON handler writes a byte that is no text as the character that stands
	// for one, so a key with such a byte and a key with that character are one name.
	t.Run("and so do keys that are one text once they are written", func(t *testing.T) {
		v, err := redact.Struct(struct{ ByKey map[string]string }{map[string]string{"k\xff": "a", "k\ufffd": "b"}})

		require.NoError(t, err)
		_, member := printed(t, v)
		assert.Equal(t, "{\"ByKey\":{\"k\ufffd\":\"a\",\"k\ufffd#2\":\"b\"}}", member)
	})

	// The handlers leave out an attribute with no name and no value, and lose
	// their place in the line when a group holds nothing else. And they take a
	// group with no name for a part of the one around it.
	t.Run("a key that is empty gets a name", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Before string
			Unset  map[string]*user
			Named  map[string]user
			After  string
		}{"b", map[string]*user{"": nil}, map[string]user{"": {"ada"}}, "a"})

		require.NoError(t, err)
		text, member := printed(t, v)
		assert.Equal(t, "v.Before=b v.Unset.[empty]=<nil> v.Named.[empty].Name=ada v.After=a", text)
		assert.Equal(t, `{"Before":"b","Unset":{"[empty]":null},"Named":{"[empty]":{"Name":"ada"}},"After":"a"}`, member)
	})
}

// card says how it is logged: who holds it, never its number.
type card struct{ Holder, Number string }

func (c card) LogValue() slog.Value { return slog.GroupValue(slog.String("holder", c.Holder)) }

// wallet says how it is logged with a value that says how it is logged.
type wallet struct {
	Owner string
	Main  card
}

func (w wallet) LogValue() slog.Value {
	return slog.GroupValue(slog.String("owner", w.Owner), slog.Any("main", w.Main))
}

// account says how it is logged with a value that is not one of slog's own.
type account struct {
	ID    string
	Login person
}

func (a account) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", a.ID), slog.Any("login", a.Login))
}

// contact says how it is logged: by its address, when it has one.
type contact struct{ Name, Email string }

func (c contact) LogValue() slog.Value {
	var email slog.Attr
	if c.Email != "" {
		email = slog.String("email", c.Email)
	}
	return slog.GroupValue(email)
}

// gauge says that it is logged as a number, and has none.
type gauge struct{}

func (gauge) LogValue() slog.Value { return slog.Float64Value(math.NaN()) }

// labelled logs itself through Struct, like profile, and holds a list.
type labelled struct{ Labels []string }

func (l labelled) LogValue() slog.Value {
	view, _ := redact.Struct(l)
	return view
}

// badge says how it is logged for its pointer alone.
type badge struct{ Holder, Number string }

func (b *badge) LogValue() slog.Value { return slog.GroupValue(slog.String("holder", b.Holder)) }

// echo says how it is logged by handing itself back.
type echo struct{}

func (e echo) LogValue() slog.Value { return slog.GroupValue(slog.Any("again", e)) }

// hydra says how it is logged by handing itself back ten times over.
type hydra struct{}

func (h hydra) LogValue() slog.Value {
	heads := make([]slog.Attr, 10)
	for i := range heads {
		heads[i] = slog.Any(strconv.Itoa(i), h)
	}
	return slog.GroupValue(heads...)
}

// loud is a struct whose String method tells more than its fields do.
type loud struct{ Quiet string }

func (loud) String() string { return canary + " from String" }

// mode is a named string with no method.
type mode string

// person is one of a roster or of a directory, which both have a text of their
// own that tells the fields of the persons in them.
type person struct{ Name, Password string }

type roster []person

func (r roster) String() string { return fmt.Sprintf("%d persons: %v", len(r), []person(r)) }

type directory map[string]person

func (d directory) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "%v", map[string]person(d)), nil
}

// tags and tally hold plain values, and have a text of their own too.
type tags []string

func (t tags) String() string { return strings.Join(t, "+") }

type tally map[string]int

func (t tally) String() string { return strconv.Itoa(len(t)) + " counted" }

// shade has a text for three values, and panics when asked for another.
type shade int

func (s shade) String() string { return [...]string{"red", "green", "blue"}[s] }

// boom panics when it is asked how it is logged.
type boom struct{}

func (boom) LogValue() slog.Value { panic(canary + " from LogValue") }

// crate says how it is logged with a value that panics when it is asked.
type crate struct{ Label string }

func (c crate) LogValue() slog.Value {
	return slog.GroupValue(slog.String("label", c.Label), slog.Any("inside", boom{}))
}

// mirror says that it is logged as itself: the answer is one more value to ask.
type mirror struct{}

func (m mirror) LogValue() slog.Value { return slog.AnyValue(m) }

// profile logs itself through Struct, the way a type with a secret would.
type profile struct {
	Name     string
	Password string
}

func (p profile) LogValue() slog.Value {
	view, _ := redact.Struct(p, "Password")
	return view
}

func TestStruct_ValuesThatSayHowTheyAreLogged(t *testing.T) {
	type event struct {
		At     time.Time
		Took   time.Duration
		Level  slog.Level
		Day    time.Weekday
		From   net.IP
		Broken net.IP
		Mode   mode
		Ratio  complex128
		Card   card
		Spare  *card
		Wallet wallet
		Loud   loud
	}

	v, err := redact.Struct(event{
		At: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), Took: 1500 * time.Millisecond,
		Level: slog.LevelWarn, Day: time.Monday, From: net.IPv4(10, 0, 0, 7), Broken: net.IP{10, 0, 7},
		Mode: "fast", Ratio: complex(1, 2),
		Card: card{Holder: "ada", Number: canary}, Spare: &card{Holder: "bob", Number: canary},
		Wallet: wallet{Owner: "cy", Main: card{Holder: "cy", Number: canary}}, Loud: loud{Quiet: "hush"},
	})

	require.NoError(t, err)
	text, member := hidden(t, v)
	assert.Equal(t, "v.At=2026-10-05T12:00:00.000Z v.Took=1.5s v.Level=WARN v.Day=Monday v.From=10.0.0.7 v.Broken=[unreadable] "+
		"v.Mode=fast v.Ratio=(1+2i) v.Card.holder=ada v.Spare.holder=bob v.Wallet.owner=cy v.Wallet.main.holder=cy v.Loud.Quiet=hush", text,
		"a time and a duration keep their type, a type with a text of its own shows it, and a struct with a String method shows its fields")
	assert.Equal(t, `{"At":"2026-10-05T12:00:00Z","Took":1500000000,"Level":"WARN","Day":"Monday","From":"10.0.0.7","Broken":"[unreadable]",`+
		`"Mode":"fast","Ratio":"(1+2i)","Card":{"holder":"ada"},"Spare":{"holder":"bob"},`+
		`"Wallet":{"owner":"cy","main":{"holder":"cy"}},"Loud":{"Quiet":"hush"}}`, member)

	// The text of a list or of a map of structs can print what a name hides, as
	// the text of a struct can.
	t.Run("a list or a map is asked for its text only when it holds plain values", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Team  roster
			Index directory
			Tags  tags
			Tally tally
		}{roster{{"ada", canary}}, directory{"k": {"bob", canary}}, tags{"a", "b"}, tally{"x": 1, "y": 2}}, "Password")

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Equal(t, "v.Team.0.Name=ada v.Team.0.Password=[redacted] v.Index.k.Name=bob v.Index.k.Password=[redacted] "+
			`v.Tags=a+b v.Tally="2 counted"`, text)
	})

	t.Run("a text that cannot be had is marked", func(t *testing.T) {
		v, err := redact.Struct(struct{ Known, Unknown shade }{1, 7})

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Equal(t, "v.Known=green v.Unknown=[unreadable]", text)
	})

	// slog turns a LogValue that panics, and one whose answers never stop being
	// values to ask, into an error. A view would show an error as the struct it
	// is, one with no field to show: the attribute would be gone.
	t.Run("and so is a value that cannot say how it is logged", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Before string
			Broken boom
			Spare  *boom
			Crate  crate
			Mirror mirror
			After  string
		}{Before: "b", Spare: &boom{}, Crate: crate{Label: "c"}, After: "a"})

		require.NoError(t, err)
		text, member := hidden(t, v)
		assert.Equal(t, "v.Before=b v.Broken=[unreadable] v.Spare=[unreadable] v.Crate.label=c v.Crate.inside=[unreadable] v.Mirror=[unreadable] v.After=a", text)
		assert.Equal(t, `{"Before":"b","Broken":"[unreadable]","Spare":"[unreadable]","Crate":{"label":"c","inside":"[unreadable]"},"Mirror":"[unreadable]","After":"a"}`, member)
	})

	t.Run("the value itself is not asked", func(t *testing.T) {
		v, err := redact.Struct(card{Holder: "ada", Number: "1234"})

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Equal(t, "v.Holder=ada v.Number=1234", text)
	})

	t.Run("so a type can log itself through Struct", func(t *testing.T) {
		var out strings.Builder
		log := slog.New(slog.NewTextHandler(&out, nil))

		log.Info("signed in", "profile", profile{Name: "ada", Password: canary})

		assert.NotContains(t, out.String(), canary)
		assert.Contains(t, out.String(), "profile.Name=ada profile.Password=[redacted]")
	})

	t.Run("and is asked when another value holds it", func(t *testing.T) {
		v, err := redact.Struct(struct{ Owner profile }{profile{Name: "ada", Password: canary}})

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Equal(t, "v.Owner.Name=ada v.Owner.Password=[redacted]", text)
	})

	// slog asks such a value through a pointer only. A view asks it wherever it
	// is held: through its address, or through a copy when it has none.
	t.Run("a type that says it for its pointer alone is asked when it is held as it is", func(t *testing.T) {
		given := struct {
			Worn   badge
			Spare  *badge
			Each   []badge
			Pair   [1]badge
			ByName map[string]badge
			Held   any
		}{
			badge{"ada", canary}, &badge{"bob", canary}, []badge{{"cy", canary}}, [1]badge{{"di", canary}},
			map[string]badge{"k": {"ed", canary}}, badge{"flo", canary},
		}
		for _, form := range []any{given, &given} {
			v, err := redact.Struct(form)

			require.NoError(t, err)
			text, _ := hidden(t, v)
			assert.Equal(t, "v.Worn.holder=ada v.Spare.holder=bob v.Each.0.holder=cy v.Pair.0.holder=di v.ByName.k.holder=ed v.Held.holder=flo", text)
		}
	})

	// What such a value hands back is shown by the rules of the view it is in.
	t.Run("a name hides an attribute of a value that logs itself", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Name    string
			Owner   profile
			Account account
		}{
			canary + "-top", profile{Name: canary + "-owner", Password: canary},
			account{ID: "7", Login: person{Name: canary + "-login", Password: canary}},
		}, "Name", "Password")

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Equal(t, "v.Name=[redacted] v.Owner.Name=[redacted] v.Owner.Password=[redacted] "+
			"v.Account.id=7 v.Account.login.Name=[redacted] v.Account.login.Password=[redacted]", text)
	})

	t.Run("what it hands back is shown like any other value", func(t *testing.T) {
		v, err := redact.Struct(struct {
			Before  string
			Nobody  contact
			Gauge   gauge
			Inner   labelled
			Account account
			After   string
		}{Before: "b", Nobody: contact{Name: "x"}, Inner: labelled{Labels: []string{"x", "y"}}, Account: account{ID: "7", Login: person{Name: "ada"}}, After: "a"})

		require.NoError(t, err)
		text, member := printed(t, v)
		assert.Equal(t, `v.Before=b v.Gauge=NaN v.Inner.Labels="[x y]" v.Account.id=7 v.Account.login.Name=ada v.Account.login.Password="" v.After=a`, text,
			"an attribute with no name and no value is left out, a number is shown as a view shows one, a list stays a list")
		assert.Equal(t, `{"Before":"b","Gauge":"NaN","Inner":{"Labels":["x","y"]},"Account":{"id":"7","login":{"Name":"ada","Password":""}},"After":"a"}`, member)
	})
}

func TestStruct_AValueThatHoldsItselfEnds(t *testing.T) {
	type node struct {
		Name     string
		Password string
		Next     *node
	}

	t.Run("a pointer that leads back is not followed again", func(t *testing.T) {
		first := &node{Name: "first", Password: canary}
		first.Next = &node{Name: "second", Password: canary, Next: first}

		v, err := redact.Struct(first, "Password")

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Equal(t, "v.Name=first v.Password=[redacted] v.Next.Name=second v.Next.Password=[redacted] v.Next.Next=[cycle]", text)
	})

	t.Run("the same value twice, side by side, is no cycle", func(t *testing.T) {
		shared := &node{Name: "shared"}

		v, err := redact.Struct(struct{ Left, Right *node }{shared, shared})

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Equal(t, "v.Left.Name=shared v.Left.Password=\"\" v.Left.Next=<nil> v.Right.Name=shared v.Right.Password=\"\" v.Right.Next=<nil>", text)
	})

	// A struct and its first field are at one address.
	t.Run("a pointer to its own first field is no cycle either", func(t *testing.T) {
		type inner struct{ N int }
		type outer struct {
			First inner
			Again *inner
		}
		given := &outer{First: inner{N: 7}}
		given.Again = &given.First

		v, err := redact.Struct(given)

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Equal(t, "v.First.N=7 v.Again.N=7", text)
	})

	t.Run("a value that hands itself back when it is asked is asked sixteen times", func(t *testing.T) {
		v, err := redact.Struct(struct{ Echo echo }{})

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Equal(t, "v.Echo."+strings.Repeat("again.", 15)+`again="[too deep]"`, text)
	})

	// Ten peers that each know the nine others have more paths between them than
	// anybody could read: only a pointer on the way down is not followed again.
	t.Run("a view stops at ten thousand values", func(t *testing.T) {
		type peer struct {
			Name     string
			Password string
			Peers    []*peer
		}
		peers := make([]*peer, 10)
		for i := range peers {
			peers[i] = &peer{Name: "p" + strconv.Itoa(i), Password: canary}
		}
		for i, one := range peers {
			one.Peers = slices.Concat(peers[:i], peers[i+1:])
		}

		v, err := redact.Struct(peers[0], "Password")

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.LessOrEqual(t, len(pairs(t, text)), 10000)
		assert.Contains(t, text, `="[too many]"`)
	})

	t.Run("and so does a list", func(t *testing.T) {
		v, err := redact.Struct(struct{ Many []int }{make([]int, 30000)})

		require.NoError(t, err)
		_, member := printed(t, v)
		var shown struct{ Many []any }
		require.NoError(t, json.Unmarshal([]byte(member), &shown))
		require.Len(t, shown.Many, 10000)
		assert.Equal(t, "[too many]", shown.Many[9999], "the last one says that there are more")
	})

	t.Run("and the answers of a value that hands itself back ten times over", func(t *testing.T) {
		v, err := redact.Struct(struct{ Hydra hydra }{})

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.LessOrEqual(t, len(pairs(t, text)), 10000)
		assert.Contains(t, text, `="[too many]"`)
	})

	t.Run("and a map, whose keys count too", func(t *testing.T) {
		many := map[int]int{}
		for i := range 30000 {
			many[i] = i
		}

		v, err := redact.Struct(struct{ Many map[int]int }{many})

		require.NoError(t, err)
		text, _ := printed(t, v)
		assert.Len(t, pairs(t, text), 5000)
		assert.Contains(t, text, `="[too many]"`)
	})

	t.Run("a chain is followed sixteen levels down", func(t *testing.T) {
		var first *node
		for i := 20; i >= 1; i-- {
			first = &node{Name: "n" + strconv.Itoa(i), Password: canary, Next: first}
		}

		v, err := redact.Struct(first, "Password")

		require.NoError(t, err)
		text, _ := hidden(t, v)
		assert.Contains(t, text, strings.Repeat("Next.", 15)+"Name=n16")
		assert.Contains(t, text, strings.Repeat("Next.", 16)+"Name=\"[too deep]\"")
		assert.NotContains(t, text, "n17")
	})
}

// everything has a place for each kind of value that a view knows how to show.
type everything struct {
	Text     string
	Password string // named when the view is made
	Key      envcfg.Secret
	Seal     sealed
	Raw      []byte
	Count    int
	Size     uint16
	Rate     float64
	Wave     complex64
	On       bool
	At       time.Time
	Took     time.Duration
	Level    slog.Level
	Do       func() string
	Done     chan string
	Next     *everything
	Twice    **everything
	Each     []everything
	Pair     [2]*everything
	ByName   map[string]*everything
	ByOwner  map[card]int
	BySecret map[envcfg.Secret]bool
	Held     any
	Rates    []float64
	Tags     []string
	Grid     [][]map[string][]*everything
}

// scatter fills v at random, up to room levels down: nil and not, empty and
// not, and among the texts and the numbers the ones a handler has to take care
// with. A string holds the canary when it must not be shown: in a field called
// Password, and when its type hides itself.
func scatter(r *rand.Rand, v reflect.Value, room int, password bool) {
	switch v.Kind() {
	case reflect.String:
		text := []string{"", "plain", "two words", "a=b", "dot.ted"}[r.IntN(5)]
		if password || v.Type() != reflect.TypeFor[string]() {
			text = canary + text
		}
		v.SetString(text)
	case reflect.Bool:
		v.SetBool(r.IntN(2) == 0)
	case reflect.Int, reflect.Int64:
		v.SetInt(r.Int64N(2000) - 1000)
	case reflect.Uint16:
		v.SetUint(r.Uint64N(1000))
	case reflect.Float64:
		v.SetFloat([]float64{0, 1.5, math.NaN(), math.Inf(1)}[r.IntN(4)])
	case reflect.Complex64:
		v.SetComplex(complex(r.Float64(), 1))
	case reflect.Array:
		for i := range v.Len() {
			scatter(r, v.Index(i), room-1, password)
		}
	case reflect.Struct:
		for field, value := range v.Fields() {
			if field.IsExported() {
				scatter(r, value, room, field.Name == "Password")
			}
		}
	case reflect.Func:
		v.Set(reflect.ValueOf(func() string { return canary }))
	case reflect.Chan:
		v.Set(reflect.MakeChan(v.Type(), 0))
	}
	if room <= 0 || r.IntN(4) == 0 {
		return // what can be nil stays nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		scatter(r, v.Elem(), room-1, password)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), r.IntN(3), 3))
		for i := range v.Len() {
			scatter(r, v.Index(i), room-1, password)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		for range r.IntN(3) {
			key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			scatter(r, key, room-1, false)
			scatter(r, value, room-1, password)
			v.SetMapIndex(key, value)
		}
	case reflect.Interface:
		v.Set(reflect.ValueOf([]any{
			7, "text", envcfg.Secret(canary), sealed(canary), (*envcfg.Secret)(nil), card{Holder: "ada", Number: canary},
			&profile{Name: "bob", Password: canary}, []any{1, "two", sealed(canary)},
			map[string]any{"Password": "a key is no field", "key": envcfg.Secret(canary)},
		}[r.IntN(9)]))
	}
}

// The tests above show what a view does with the values they picked. This one
// fills a struct at random, three hundred times, and asks of each view what
// must hold for all of them: both handlers write the same keys, in lines that
// can be read back, and the canary is in none.
func TestStruct_WhateverTheValueHolds(t *testing.T) {
	r := rand.New(rand.NewPCG(2026, 10))
	for round := range 300 {
		var given everything
		scatter(r, reflect.ValueOf(&given).Elem(), 3, false)
		if given.Next != nil && round%3 == 0 {
			given.Next.Next = &given // it holds itself
		}

		v, err := redact.Struct(&given, "Password")

		require.NoError(t, err)
		_, member := hidden(t, v)
		assert.NotContains(t, member, "!ERROR", "the JSON handler could not write a value")
		if t.Failed() {
			t.Fatalf("round %d", round)
		}
	}
}

// The helper does not import envcfg, and a Secret hides itself: the two have to
// write the same text.
func TestPlaceholderIsTheOneASecretPrints(t *testing.T) {
	secret := envcfg.Secret("x")

	assert.Equal(t, redact.Placeholder, fmt.Sprint(secret))
	assert.Equal(t, redact.Placeholder, secret.LogValue().String())
}

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"nothing to drop", "http://localhost:9093/api", "http://localhost:9093/api"},
		{"a user and a password", "https://svc:" + canary + "@agent.example.com/api", "https://agent.example.com/api"},
		{"a user alone", "https://" + canary + "@chat.example.com", "https://chat.example.com"},
		{"a query", "https://agent.example.com/api?key=" + canary, "https://agent.example.com/api"},
		{"a fragment", "https://agent.example.com/api#" + canary, "https://agent.example.com/api"},
		{"a host and no scheme", "//svc:" + canary + "@agent.example.com/api", "//agent.example.com/api"},
		{"an address that cannot be read", "https://svc:" + canary + "@agent.example.com:port/api", "unreadable"},
		{"no address", "", ""},
		// A slash ends the host. One in a password that was not encoded moves the
		// rest of the password, and the host that was meant, into the path.
		{"a password with a slash after an @", "postgres://app:P@ss/" + canary + "@localhost:5440/agentwebui", "unreadable"},
		{"a password that starts with a slash", "postgres://app:/" + canary + "@localhost:5440/agentwebui", "unreadable"},
		{"a password with a slash after digits", "postgres://app:2024/" + canary + "@localhost:5440/agentwebui", "unreadable"},
		// A question mark and a hash sign end the host too, and digits before one are taken for a port.
		{"a password with a question mark after digits", "https://svc:2024?" + canary + "@agent.example.com/api", "unreadable"},
		{"a password with a hash sign after digits", "https://svc:2024#" + canary + "@agent.example.com/api", "unreadable"},
		{"a user and a host with no scheme", canary + "@agent.example.com", "unreadable"},
		{"a path alone", "/var/run/" + canary, "unreadable"},
		{"what is no address", "host=localhost user=app password=" + canary, "unreadable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, redact.URL(tc.raw))
		})
	}
}

func ExampleStruct() {
	type database struct {
		Host     string
		User     string
		Password string
	}
	type settings struct {
		Env      string
		Database database
		APIKey   envcfg.Secret
	}
	loaded := settings{Env: "production", Database: database{Host: "db.internal", User: "app", Password: "hunter2"}, APIKey: "sk-123"}

	view, err := redact.Struct(loaded, "Password")
	if err != nil {
		fmt.Println(err)
		return
	}
	withoutTime := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: withoutTime})).Info("starting", "settings", view)

	// Output:
	// level=INFO msg=starting settings.Env=production settings.Database.Host=db.internal settings.Database.User=app settings.Database.Password=[redacted] settings.APIKey=[redacted]
}
