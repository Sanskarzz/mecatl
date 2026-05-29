---
name: go-table-tests
description: Write idiomatic table-driven Go tests that stay offline and follow this repo's conventions.
---

# Go table-driven tests

When adding tests in this codebase:

1. **Stay offline.** Use `mockllm` + `memfs` (or a `t.TempDir()` for file-backed
   adapters). Never hit a live model or the network in a test.
2. **Prefer table-driven cases** for any behaviour with multiple inputs:

   ```go
   func TestThing(t *testing.T) {
       cases := []struct {
           name string
           in   string
           want string
       }{
           {name: "empty", in: "", want: ""},
           {name: "basic", in: "x", want: "X"},
       }
       for _, tc := range cases {
           t.Run(tc.name, func(t *testing.T) {
               if got := Thing(tc.in); got != tc.want {
                   t.Errorf("Thing(%q) = %q, want %q", tc.in, got, tc.want)
               }
           })
       }
   }
   ```

3. **Assert error results, not panics.** Tool-level failures come back as an
   error `ToolResult` (`res.IsError`), not a Go error — check the result.
4. Run the suite with `task test` (which uses `-race`).
