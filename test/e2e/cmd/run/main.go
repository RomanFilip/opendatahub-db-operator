package main

import (
	"os"

	"github.com/opendatahub-io/opendatahub-db-operator/test/e2e/runner"
)

func main() {
	r := runner.New()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			os.Exit(r.Setup())
		case "teardown":
			os.Exit(r.Teardown())
		}
	}
	os.Exit(r.Run(os.Args[1:]).ExitCode)
}
