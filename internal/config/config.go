// Package config reads runtime settings from environment variables.
//
// Env-only configuration (no flags, no config file) keeps the container image
// identical across environments: the same image runs locally, in compose and
// in any future orchestrator, differing only in the env it is given.
package config

import (
	"os"
	"strings"
)

type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// DBPath is the SQLite file. Defaults inside ./data so a compose volume can
	// be mounted there and history survives container restarts.
	DBPath string
	// SeedUsers are created on boot so a demo needs no sign-up step.
	SeedUsers    []string
	GroupName    string
	GroupMembers []string
}

func FromEnv() Config {
	c := Config{
		Addr:   ":" + getenv("PORT", "8080"),
		DBPath: getenv("DB_PATH", "data/chat.db"),
	}
	if s := os.Getenv("SEED_USERS"); s != "" {
		for _, u := range strings.Split(s, ",") {
			if u = strings.TrimSpace(u); u != "" {
				c.SeedUsers = append(c.SeedUsers, u)
			}
		}
	}

	if groupName := os.Getenv("GROUP_NAME"); groupName != "" {
		c.GroupName = groupName
	}

	if gm := os.Getenv("GROUP_MEMBERS"); gm != "" {
		for _, gms := range strings.Split(gm, ",") {
			if gms = strings.TrimSpace(gms); gms != "" {
				c.GroupMembers = append(c.GroupMembers, gms)
			}
		}
	}

	return c
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
