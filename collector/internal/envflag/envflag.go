// Package envflag lets every command-line flag also be set through an
// environment variable. It has no dependencies, so commands that don't
// touch GPUs (the hub) build without cgo.
package envflag

import (
	"flag"
	"fmt"
	"strings"
)

// Apply sets every flag not given on the command line from its
// environment variable: prefix + the flag name in upper case with "-" as "_".
func Apply(fs *flag.FlagSet, prefix string, getenv func(string) string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || err != nil {
			return
		}
		env := prefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v := getenv(env); v != "" {
			if e := fs.Set(f.Name, v); e != nil {
				err = fmt.Errorf("%s=%q: %w", env, v, e)
			}
		}
	})
	return err
}
