package runtime

// hostpaths.go — ONE PLACE EVERY HOST PATH CROSSING INTO A CONTAINER IS CONVERTED.
//
// THE FAILURE THIS EXISTS FOR. A worker in a runtime container reported, verbatim:
//
//	runtime exec: chdir C:\projects\weatherapp\.orchicon-worktrees\01M69SPR...:
//	no such file or directory
//
// `echo hi`, `pwd`, `exit 0` — every command, so no node --test, no git, no push. The cause was one field:
// AgentRequest.Cwd was forwarded verbatim, while ProjectDir beside it was already mapped. A Linux container
// has no C:\ , so the chdir could only fail.
//
// AND IT LEFT PHYSICAL DEBRIS. Two directories appeared in the checkout, named for the paths with the colon
// and the backslashes gone:
//
//	C:\projects\weatherapp\Cprojectsweatherapp
//	C:\projects\weatherapp\Cprojectsweatherapp.orchicon-worktrees01M69NP73...
//
// Those are what an in-container process creates when it is handed "C:\projects\weatherapp" and treats it as
// a RELATIVE path: mkdir -p makes a directory of that literal name, and Windows cannot represent ':' or '\'
// in a file name, so the share shows the characters dropped. Each contains a .orchicon/ subdir, i.e. the
// sandbox/memory directory, which is created as a side effect. THE DEBRIS IS A SYMPTOM: nothing in a
// container can invent that directory if a Windows path never reaches it.
//
// WHY A TAG AND NOT A LIST. Six separate bugs — allowlist compare, -v spec, --tmpfs spec, the entrypoint, the
// worktree .git pointer, and this cwd — were all "a host path crossed a boundary unmapped". Each was found by
// a report, one round trip at a time, because the set of paths was held in the knowledge of whoever last
// edited the code. The set now lives ON THE STRUCT, where adding a path field and forgetting to map it is the
// same edit:
//
//	Cwd string `json:"cwd,omitempty" orchicon:"hostpath"`
//
// mapHostPathFields walks exactly those tags, so a new field is mapped by having the tag, and
// TestEveryTaggedHostPathIsMapped fails if a tagged field is ever missed.

import (
	"reflect"
	"strings"
)

// hostPathTag marks a struct field that carries a HOST path which must be expressed in the engine's form
// before the value crosses into a container.
const hostPathTag = "orchicon"

// hostPathTagValue is the value of that tag on a path-bearing field.
const hostPathTagValue = "hostpath"

// mapHostPathFields rewrites every field tagged `orchicon:"hostpath"` on v into the container's form.
//
// v must be a pointer to a struct. Nested structs and slices are NOT descended: a mount spec is handled at
// the argv boundary by guestifyCreateArgv, which has to parse `src:dst:opts` anyway and so cannot share this
// walk. The fields this covers are the plain string ones — which is exactly the shape the reported bug had.
func mapHostPathFields(m *PathMapper, v any) {
	if m == nil || v == nil {
		return
	}
	if m.Mode() == PathModeIdentity {
		// Linux: the engine shares this filesystem namespace, so there is nothing to convert. Returning here
		// keeps the walk off the hot path on the platform that needs none of it.
		return
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Tag.Get(hostPathTag) != hostPathTagValue {
			continue
		}
		fv := rv.Field(i)
		if fv.Kind() != reflect.String || !fv.CanSet() {
			continue
		}
		if cur := strings.TrimSpace(fv.String()); cur != "" {
			fv.SetString(m.Map(cur))
		}
	}
}

// taggedHostPathFields lists the field names a type declares as host paths. Used by the audit test, and by
// nothing in production — it exists so the property can be asserted rather than assumed.
func taggedHostPathFields(t reflect.Type) []string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get(hostPathTag) == hostPathTagValue {
			out = append(out, t.Field(i).Name)
		}
	}
	return out
}
