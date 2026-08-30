package config

import "testing"

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("DB_PATH", "")
	t.Setenv("SEED_USERS", "")
	c := FromEnv()
	if c.Addr != ":8080" || c.DBPath != "data/chat.db" || len(c.SeedUsers) != 0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestFromEnvSeedUsers(t *testing.T) {
	t.Setenv("SEED_USERS", " alice, bob ,,")
	c := FromEnv()
	if len(c.SeedUsers) != 2 || c.SeedUsers[0] != "alice" || c.SeedUsers[1] != "bob" {
		t.Fatalf("seed users not trimmed/split: %v", c.SeedUsers)
	}
}
