// Comando api: servidor HTTP e workers, compostos com Uber Fx.
package main

import (
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/feliphexavier/jungleGamingProjeto/internal/bootstrap"
	"github.com/feliphexavier/jungleGamingProjeto/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Run bloqueia até SIGINT/SIGTERM e então executa os OnStop em ordem inversa.
	fx.New(bootstrap.Options(cfg)).Run()
}
