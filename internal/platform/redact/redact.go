// Package redact turns a struct into a view that a log can show: its exported
// fields as a slog group, with the ones that hold a secret hidden. It knows
// nothing of this module: a value is hidden because the caller names its field,
// or because it says how it is logged, as a Secret does.
package redact

import (
	"cmp"
	"encoding"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Placeholder stands in for a value that is not shown. It is the text a Secret prints.
const Placeholder = "[redacted]"

// maxDepth is how many levels of fields, elements and entries a view follows.
const maxDepth = 16

// maxValues is how many values a view shows. Parts that point at each other
// have more paths between them than a log line can hold: only a pointer on the
// way down is not followed again.
const maxValues = 10_000

// maxAnswers is how many times in a row a view asks how a value is logged
// while each answer is one more value to ask. slog.Value.Resolve stops at the
// same number.
const maxAnswers = 100

// Struct returns v, a struct or a pointer to one, as a slog group: one attribute
// for each exported field, under the name of the field. A field named in hide
// shows the placeholder, wherever a field of that name sits in v. When a name is
// no field, the error says so and the value is the placeholder alone, so a
// caller that drops the error shows nothing of v. A nil pointer to a struct is
// no error: its names are checked, and the value is nil and no group.
//
// What the view shows of the values in v:
//   - A struct is a group of its own, and so is a map, or a slice or an array
//     of anything but plain values, with the key or the index for a name. slog
//     leaves a group with nothing in it out, so an empty one has no attribute.
//   - A value that can hold no field shows the text its type has for it, of
//     MarshalText or of String: a level, an address. Bytes with no such text
//     show as their number.
//   - A pointer shows what it points at: nil when that is nothing, [cycle]
//     when it leads back to a value the view is already inside.
//   - A value that implements slog.LogValuer is asked how it is logged, which
//     is how a Secret stays hidden without being named. What it answers is
//     shown by these same rules: a name hides an attribute of that name in it.
//     One that has no answer shows [unreadable]: its LogValue panics, or
//     never stops handing back one more value to ask.
//   - A String method of a struct is not asked, nor that of a list or a map of
//     structs, since it can print what the view hides.
//   - A field that is not exported is left out, whatever it holds: the type
//     keeps it to itself, and nothing can ask it how it is logged.
//
// The view goes sixteen levels down and shows ten thousand values, and says
// [too deep] or [too many] where it stops.
//
// The LogValue of v itself is not asked, so a type can call Struct from its
// own. A call knows the pointers of its own view only: values that hold each
// other in a circle and each log themselves through Struct never end.
//
// Struct hides what is named and what hides itself, and shows the rest: a plain
// string that holds a secret is shown unless its field is named. A name is the
// name of one exported field, not a path. It is checked against the types
// alone, so a field that only a value behind an interface has cannot be named:
// name the field that holds the interface.
func Struct(v any, hide ...string) (slog.Value, error) {
	nothing := slog.StringValue(Placeholder)
	if v == nil {
		return nothing, errors.New("redact: nil is not a struct")
	}
	given := reflect.ValueOf(v)
	t := given.Type()
	for range maxDepth { // a pointer type can point at itself
		if t.Kind() != reflect.Pointer {
			break
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nothing, fmt.Errorf("redact: %s is not a struct", given.Type())
	}
	if err := checkNames(t, hide); err != nil {
		return nothing, err
	}
	w := walk{hide: hide, inside: map[pointer]bool{}, left: maxValues}
	for given.Kind() == reflect.Pointer {
		if given.IsNil() {
			return slog.AnyValue(nil), nil
		}
		w.inside[pointerOf(given)] = true
		given = given.Elem()
	}
	return w.fields(given, 0), nil
}

// Unreadable is what URL gives for an address that it cannot show.
const Unreadable = "unreadable"

// URL is where a URL points: its scheme, host and path, without the user, the
// password or the query it may carry. It is Unreadable when it cannot be
// read, when it has no host, and when it has an @ after its host: a slash, a
// question mark or a hash sign ends the host, so a password that holds one
// and was not encoded leaves a part of itself, and the host that was meant,
// before or after that sign. No address is no address.
func URL(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case raw == "":
		return ""
	case err != nil, u.Host == "", strings.Contains(u.Path+u.RawQuery+u.Fragment, "@"):
		return Unreadable
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// checkNames reports the names that are no field a view of t can show: no
// exported field of t, nor of a struct that t leads to. Only the types are
// read, so a name is known also when the value that would hold its field is
// nil. A field that is not exported is no name: it would let through the typo
// that is spelled like a field some type keeps to itself.
func checkNames(t reflect.Type, names []string) error {
	known := map[string]bool{}
	fieldNames(t, known, map[reflect.Type]bool{})
	var unknown []error
	for _, name := range names {
		if !known[name] {
			unknown = append(unknown, fmt.Errorf("redact: no exported field %q in %s or in a struct inside it", name, t))
		}
	}
	return errors.Join(unknown...)
}

// fieldNames adds to names the exported fields of every struct that t leads to
// through exported fields, pointers, slices, arrays and maps.
func fieldNames(t reflect.Type, names map[string]bool, seen map[reflect.Type]bool) {
	if seen[t] {
		return
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Map:
		fieldNames(t.Key(), names, seen)
		fieldNames(t.Elem(), names, seen)
	case reflect.Pointer, reflect.Slice, reflect.Array:
		fieldNames(t.Elem(), names, seen)
	case reflect.Struct:
		for field := range t.Fields() {
			if field.IsExported() {
				names[field.Name] = true
				fieldNames(field.Type, names, seen)
			}
		}
	}
}

// walk makes one view.
type walk struct {
	hide []string
	// inside holds the pointers followed on the way down to the value at hand.
	inside map[pointer]bool
	// left is how many more values the view has room for.
	left int
}

// full reports whether the view has room for no more values.
func (w *walk) full() bool { return w.left < 0 }

// pointer is where a pointer leads and to what type: a struct and its first
// field are at one address, and are not one value.
type pointer struct {
	to   reflect.Type
	addr uintptr
}

func pointerOf(v reflect.Value) pointer { return pointer{to: v.Type(), addr: v.Pointer()} }

// fields is the group of a struct: its exported fields in their order, with
// the placeholder for the ones that are named.
func (w *walk) fields(v reflect.Value, depth int) slog.Value {
	attrs := make([]slog.Attr, 0, v.NumField())
	for field, value := range v.Fields() {
		switch {
		case w.full():
		case !field.IsExported():
			// The type keeps it to itself.
		case slices.Contains(w.hide, field.Name):
			attrs = append(attrs, slog.String(field.Name, Placeholder))
		default:
			attrs = append(attrs, slog.Attr{Key: field.Name, Value: w.value(value, depth+1)})
		}
	}
	return slog.GroupValue(attrs...)
}

// value is how one value is shown, depth levels below the struct that was given.
func (w *walk) value(v reflect.Value, depth int) slog.Value {
	w.left--
	switch {
	case w.full():
		return slog.StringValue("[too many]")
	case depth > maxDepth:
		return slog.StringValue("[too deep]")
	case canBeNil(v.Kind()) && v.IsNil():
		return slog.AnyValue(nil)
	case v.Kind() == reflect.Interface:
		return w.value(v.Elem(), depth)
	case v.Kind() == reflect.Pointer && w.inside[pointerOf(v)]:
		return slog.StringValue("[cycle]")
	}
	if self, ok := selfLogging(v); ok {
		return w.said(slog.AnyValue(self), depth)
	}
	switch v.Kind() {
	case reflect.Pointer:
		return w.pointee(v, depth)
	case reflect.Struct:
		if at, ok := reflect.TypeAssert[time.Time](v); ok {
			return instant(at)
		}
		return w.fields(v, depth)
	}
	if lasts, ok := reflect.TypeAssert[time.Duration](v); ok {
		return slog.DurationValue(lasts)
	}
	if text, ok := ownText(v); ok {
		return slog.StringValue(text)
	}
	return w.plain(v, depth)
}

func canBeNil(k reflect.Kind) bool {
	return k == reflect.Pointer || k == reflect.Interface || k == reflect.Map || k == reflect.Slice
}

// selfLogging is v as a value that says how it is logged, when it is one. A
// type may say it for its pointer alone. A value of such a type that is held
// as it is gets asked too, through its address, or through a copy when it has
// none: slog would show it field by field.
func selfLogging(v reflect.Value) (slog.LogValuer, bool) {
	if self, ok := reflect.TypeAssert[slog.LogValuer](v); ok {
		return self, true
	}
	if !reflect.PointerTo(v.Type()).Implements(reflect.TypeFor[slog.LogValuer]()) {
		return nil, false
	}
	if !v.CanAddr() {
		held := reflect.New(v.Type()).Elem()
		held.Set(v)
		v = held
	}
	return reflect.TypeAssert[slog.LogValuer](v.Addr())
}

// said is what a value that says how it is logged hands back, shown by the
// rules of the view it is in. Whatever in it says how it is logged is asked in
// turn, so that whoever prints the view finds nothing left to ask. A name hides
// an attribute as it hides a field, and a value that is not one of slog's own
// is walked like any other. A value that has no answer leaves [unreadable].
func (w *walk) said(v slog.Value, depth int) slog.Value {
	w.left--
	switch {
	case w.full():
		return slog.StringValue("[too many]")
	case depth > maxDepth:
		return slog.StringValue("[too deep]")
	}
	v, ok := answer(v)
	if !ok {
		return slog.StringValue("[unreadable]")
	}
	switch v.Kind() {
	case slog.KindGroup:
		attrs := make([]slog.Attr, 0, len(v.Group()))
		for _, attr := range v.Group() {
			switch {
			case w.full():
			case attr.Equal(slog.Attr{}):
				// The handlers leave it out, and lose their place in the line
				// when a group holds nothing else.
			case slices.Contains(w.hide, attr.Key):
				attrs = append(attrs, slog.String(attr.Key, Placeholder))
			default:
				attrs = append(attrs, slog.Attr{Key: attr.Key, Value: w.said(attr.Value, depth+1)})
			}
		}
		return slog.GroupValue(attrs...)
	case slog.KindFloat64:
		return number(v.Float64())
	case slog.KindTime:
		return instant(v.Time())
	case slog.KindAny:
		if _, ours := v.Any().(plainList); !ours && v.Any() != nil {
			return w.value(reflect.ValueOf(v.Any()), depth)
		}
	}
	return v
}

// answer is what v says when it is asked how it is logged, asked again while
// what it says is one more value to ask, and v itself when it is none to ask.
// A LogValue that panics has no answer, nor has one whose answers never end.
// slog.Value.Resolve asks the same way and hands back an error for the two,
// which a view would show as the struct it is, one with no field to show: so
// the view asks under a recover of its own, as ownText does.
func answer(v slog.Value) (given slog.Value, ok bool) {
	defer func() {
		if recover() != nil {
			given, ok = slog.Value{}, false
		}
	}()
	for range maxAnswers {
		if v.Kind() != slog.KindLogValuer {
			return v, true
		}
		v = v.LogValuer().LogValue()
	}
	return slog.Value{}, false
}

// pointee is what a pointer leads to, at the pointer's own level.
func (w *walk) pointee(v reflect.Value, depth int) slog.Value {
	at := pointerOf(v)
	w.inside[at] = true
	defer delete(w.inside, at)
	return w.value(v.Elem(), depth)
}

// ownText is the text a value has for itself: what its MarshalText gives, or
// its String. Only a value that can hold no field is asked, such as a level or
// an address held as bytes: the text of a list or of a map of structs can print
// what the view hides, as the text of a struct can. A method that fails or
// panics leaves [unreadable].
func ownText(v reflect.Value) (text string, has bool) {
	if !holdsNoField(v.Type()) {
		return "", false
	}
	defer func() {
		if recover() != nil {
			text, has = "[unreadable]", true
		}
	}()
	if marshaler, ok := reflect.TypeAssert[encoding.TextMarshaler](v); ok {
		raw, err := marshaler.MarshalText()
		if err != nil {
			return "[unreadable]", true
		}
		return string(raw), true
	}
	if stringer, ok := reflect.TypeAssert[fmt.Stringer](v); ok {
		return stringer.String(), true
	}
	return "", false
}

// holdsNoField reports whether a value of t, which is no struct, pointer or
// interface, holds nothing but plain values.
func holdsNoField(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return isPlain(t.Elem())
	case reflect.Map:
		return isPlain(t.Key()) && isPlain(t.Elem())
	}
	return true
}

// plain is how a value with no say of its own is shown: by its kind.
func (w *walk) plain(v reflect.Value, depth int) slog.Value {
	switch {
	case v.Kind() == reflect.Bool:
		return slog.BoolValue(v.Bool())
	case v.Kind() == reflect.String:
		return slog.StringValue(v.String())
	case v.CanInt():
		return slog.Int64Value(v.Int())
	case v.CanUint():
		return slog.Uint64Value(v.Uint())
	case v.CanFloat():
		return number(v.Float())
	case v.CanComplex():
		return slog.StringValue(strconv.FormatComplex(v.Complex(), 'g', -1, 128))
	case v.Kind() == reflect.Slice || v.Kind() == reflect.Array:
		return w.list(v, depth)
	case v.Kind() == reflect.Map:
		return w.entries(v, depth)
	}
	return slog.StringValue("[" + v.Kind().String() + "]")
}

// number is how a float is shown: as it is, or as its text when JSON has no
// number for it, since the JSON handler could not write it.
func number(f float64) slog.Value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return slog.StringValue(strconv.FormatFloat(f, 'g', -1, 64))
	}
	return slog.Float64Value(f)
}

// instant is how a time is shown: as it is, or as its text when its year has
// not four digits, since the handlers could not write it.
func instant(at time.Time) slog.Value {
	if at.Year() < 0 || at.Year() > 9999 {
		return slog.StringValue(at.String())
	}
	return slog.TimeValue(at)
}

// plainList is a list of plain values in a view. It has a type of its own, so
// that a view knows the lists it made when a value that logs itself through
// Struct hands one back.
type plainList []any

// list is how a slice or an array is shown: bytes as their number, plain values
// as the list they are, anything else as a group with the index for a name.
func (w *walk) list(v reflect.Value, depth int) slog.Value {
	of := v.Type().Elem()
	if of.Kind() == reflect.Uint8 {
		return slog.StringValue("[" + strconv.Itoa(v.Len()) + " bytes]")
	}
	if isPlain(of) {
		values := plainList{}
		for i := 0; i < v.Len() && !w.full(); i++ {
			values = append(values, w.value(v.Index(i), depth+1).Any())
		}
		return slog.AnyValue(values)
	}
	var items []slog.Attr
	for i := 0; i < v.Len() && !w.full(); i++ {
		items = append(items, slog.Attr{Key: strconv.Itoa(i), Value: w.value(v.Index(i), depth+1)})
	}
	return slog.GroupValue(items...)
}

// isPlain reports whether t is a truth value, a number or a string with no
// name of its own, so that its values have no method to ask.
func isPlain(t reflect.Type) bool {
	if t.PkgPath() != "" {
		return false
	}
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}

// entries is how a map is shown: a group with the keys as text for names,
// sorted. A key whose text is empty is named [empty]: the handlers of slog
// leave out an attribute with no name and no value, and take a group with no
// name for a part of the group around it. Keys with one text, as two secrets
// have, are told apart by a number, so that no name is in a line twice; a byte
// that is no text counts as the character the JSON handler writes for it.
func (w *walk) entries(v reflect.Value, depth int) slog.Value {
	var entries []slog.Attr
	for key, value := range v.Seq2() {
		if w.full() {
			break
		}
		name := cmp.Or(strings.ToValidUTF8(w.value(key, depth+1).String(), "\uFFFD"), "[empty]")
		entries = append(entries, slog.Attr{Key: name, Value: w.value(value, depth+1)})
	}
	slices.SortFunc(entries, func(a, b slog.Attr) int {
		if byName := strings.Compare(a.Key, b.Key); byName != 0 {
			return byName
		}
		return strings.Compare(a.Value.String(), b.Value.String())
	})
	taken := map[string]bool{}
	for i, entry := range entries {
		name := entry.Key
		for n := 2; taken[name]; n++ {
			name = entry.Key + "#" + strconv.Itoa(n)
		}
		taken[name] = true
		entries[i].Key = name
	}
	return slog.GroupValue(entries...)
}
