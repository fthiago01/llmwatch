package auth

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	BcryptCost = bcrypt.MinCost
	os.Exit(m.Run())
}
