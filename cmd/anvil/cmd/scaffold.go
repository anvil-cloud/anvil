package cmd

import (
	"fmt"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd/scaffold"
)

// scaffoldFunctions creates the code for declared functions that don't exist
// yet (today: Rust Lambdas) and prints what it created. It runs in
// anvil generate and in anvil deploy before the build, since a missing
// package can't be built. Existing code is never touched.
//
// dbCrate is the generated DSQL client crate, if any: new Rust Lambdas get a
// dependency on it.
func scaffoldFunctions(projectRoot string, functions []FunctionSpec, dbCrate string) error {
	var rust []scaffold.RustFunction
	for _, fn := range functions {
		if fn.Type == "anvil:aws:Lambda" && fn.Runtime == rustRuntime {
			rust = append(rust, scaffold.RustFunction{Name: fn.Name, Handler: fn.Entry})
		}
	}
	if len(rust) == 0 {
		return nil
	}

	created, err := scaffold.Rust(projectRoot, rust, scaffold.RustOptions{DBCrate: dbCrate})
	for _, c := range created {
		fmt.Printf("  created %s\n", c)
	}
	return err
}

// hasRustFunctions reports whether any discovered function is a Rust Lambda.
func hasRustFunctions(functions []FunctionSpec) bool {
	for _, fn := range functions {
		if fn.Type == "anvil:aws:Lambda" && fn.Runtime == rustRuntime {
			return true
		}
	}
	return false
}
