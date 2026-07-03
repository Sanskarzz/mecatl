package ui

import (
	"reflect"
	"testing"
)

// keyMapFieldNames returns the exported field names of the keyMap struct via
// reflection — the canonical set of rebindable action names.
func keyMapFieldNames() []string {
	tp := reflect.TypeOf(keyMap{})
	names := make([]string, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		names = append(names, tp.Field(i).Name)
	}
	return names
}

// TestEveryKeyHasOverrideSetter pins the rebindable-key parity invariant (issue #228
// added EditBack): EVERY keyMap field must have an applyKeyOverrides setter, or that
// key silently becomes non-rebindable. We probe the setter by overriding each action
// to a distinct sentinel chord and asserting applyKeyOverrides actually rebound it.
func TestEveryKeyHasOverrideSetter(t *testing.T) {
	base := defaultKeys()
	for _, name := range keyMapFieldNames() {
		name := name
		t.Run(name, func(t *testing.T) {
			const sentinel = "ctrl+f19" // a chord no default binding uses
			got := applyKeyOverrides(base, map[string][]string{name: {sentinel}})
			binding := reflect.ValueOf(got).FieldByName(name)
			// key.Binding.Keys() is the current chord set; assert it now contains the
			// sentinel — i.e. the setter fired for this field.
			out := binding.MethodByName("Keys").Call(nil)
			keys, _ := out[0].Interface().([]string)
			found := false
			for _, k := range keys {
				if k == sentinel {
					found = true
				}
			}
			if !found {
				t.Fatalf("keyMap field %q has no applyKeyOverrides setter (override did not take): keys=%v", name, keys)
			}
		})
	}
}
