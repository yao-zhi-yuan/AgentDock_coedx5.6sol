package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/agentdock/agentdock-verify/internal/migration"
)

func main() {
	databaseURL := flag.String("database-url", "", "PostgreSQL connection URL")
	path := flag.String("path", "migrations", "SQL migration directory")
	flag.Parse()
	if err := migration.Up(*databaseURL, *path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("migrations applied")
}
