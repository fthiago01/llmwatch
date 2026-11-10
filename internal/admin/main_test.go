package admin

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"llmwatch/internal/auth"
)

func TestMain(m *testing.M) {
	auth.BcryptCost = bcrypt.MinCost
	os.Exit(m.Run())
}
