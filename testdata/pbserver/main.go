package main

import (
	"log"
	"os"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"

	"github.com/Barney241/pocketbase-cli/pbguard"
)

func main() {
	app := pocketbase.New()
	jsvm.MustRegister(app, jsvm.Config{HooksDir: os.Getenv("PBSERVER_HOOKS_DIR")})
	if emails := os.Getenv("PBSERVER_GO_GUARD"); emails != "" {
		app.OnServe().BindFunc(func(se *core.ServeEvent) error {
			pbguard.Bind(se, strings.Split(emails, ",")...)
			return se.Next()
		})
	}
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
