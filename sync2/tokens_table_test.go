package sync2

import (
	"testing"
	"time"
)

// Sanity check that different tokens have different hashes
func TestHash(t *testing.T) {
	token1 := "ABCD"
	token2 := "EFGH"
	hash1 := hashToken(token1)
	hash2 := hashToken(token2)
	if hash1 == hash2 {
		t.Fatalf("HashedTokenFromRequest: %s and %s have the same hash", token1, token2)
	}
}

func TestTokensTable(t *testing.T) {
	db, close := connectToDB(t)
	defer close()
	tokens := NewTokensTable(db, "my_secret")

	alice := "@alice:localhost"
	aliceDevice := "alice_phone"
	aliceSecret1 := "mysecret1"
	aliceToken1FirstSeen := time.Now()

	// Test a single token
	t.Log("Insert a new token from Alice.")
	aliceToken, err := tokens.Insert(aliceSecret1, alice, aliceDevice, aliceToken1FirstSeen)
	if err != nil {
		t.Fatalf("Failed to Insert token: %s", err)
	}

	t.Log("The returned Token struct should have been populated correctly.")
	assertEqualTokens(t, tokens, aliceToken, aliceSecret1, alice, aliceDevice, aliceToken1FirstSeen)

	t.Log("Reinsert the same token.")
	reinsertedToken, err := tokens.Insert(aliceSecret1, alice, aliceDevice, aliceToken1FirstSeen)
	if err != nil {
		t.Fatalf("Failed to Insert token: %s", err)
	}

	t.Log("This should yield an equal Token struct.")
	assertEqualTokens(t, tokens, reinsertedToken, aliceSecret1, alice, aliceDevice, aliceToken1FirstSeen)

	t.Log("Try to mark Alice's token as being used after an hour.")
	err = tokens.MaybeUpdateLastSeen(aliceToken, aliceToken1FirstSeen.Add(time.Hour))
	if err != nil {
		t.Fatalf("Failed to update last seen: %s", err)
	}

	t.Log("The token should not be updated in memory, nor in the DB.")
	assertEqualTimes(t, aliceToken.LastSeen, aliceToken1FirstSeen, "Token.LastSeen mismatch")
	fetchedToken, err := tokens.Token(aliceSecret1)
	if err != nil {
		t.Fatalf("Failed to fetch token: %s", err)
	}
	assertEqualTokens(t, tokens, fetchedToken, aliceSecret1, alice, aliceDevice, aliceToken1FirstSeen)

	t.Log("Try to mark Alice's token as being used after two days.")
	aliceToken1LastSeen := aliceToken1FirstSeen.Add(48 * time.Hour)
	err = tokens.MaybeUpdateLastSeen(aliceToken, aliceToken1LastSeen)
	if err != nil {
		t.Fatalf("Failed to update last seen: %s", err)
	}

	t.Log("The token should now be updated in-memory and in the DB.")
	assertEqualTimes(t, aliceToken.LastSeen, aliceToken1LastSeen, "Token.LastSeen mismatch")
	fetchedToken, err = tokens.Token(aliceSecret1)
	if err != nil {
		t.Fatalf("Failed to fetch token: %s", err)
	}
	assertEqualTokens(t, tokens, fetchedToken, aliceSecret1, alice, aliceDevice, aliceToken1LastSeen)

	// Test a second token for Alice
	t.Log("Insert a second token for Alice.")
	aliceSecret2 := "mysecret2"
	aliceToken2FirstSeen := aliceToken1LastSeen.Add(time.Minute)
	aliceToken2, err := tokens.Insert(aliceSecret2, alice, aliceDevice, aliceToken2FirstSeen)
	if err != nil {
		t.Fatalf("Failed to Insert token: %s", err)
	}

	t.Log("The returned Token struct should have been populated correctly.")
	assertEqualTokens(t, tokens, aliceToken2, aliceSecret2, alice, aliceDevice, aliceToken2FirstSeen)
}

func TestDeletingTokens(t *testing.T) {
	db, close := connectToDB(t)
	defer close()
	tokens := NewTokensTable(db, "my_secret")

	t.Log("Insert a new token from Alice.")
	accessToken := "mytoken"
	token, err := tokens.Insert(accessToken, "@bob:builders.com", "device", time.Time{})
	if err != nil {
		t.Fatalf("Failed to Insert token: %s", err)
	}

	t.Log("We should be able to fetch this token without error.")
	_, err = tokens.Token(accessToken)
	if err != nil {
		t.Fatalf("Failed to fetch token: %s", err)
	}

	t.Log("Delete the token")
	err = tokens.Delete(token.AccessTokenHash)

	if err != nil {
		t.Fatalf("Failed to delete token: %s", err)
	}

	t.Log("We should no longer be able to fetch this token.")
	token, err = tokens.Token(accessToken)
	if token != nil || err == nil {
		t.Fatalf("Fetching token after deletion did not fail: got %s, %s", token, err)
	}
}

func assertEqualTokens(t *testing.T, table *TokensTable, got *Token, accessToken, userID, deviceID string, lastSeen time.Time) {
	t.Helper()
	assertEqual(t, got.AccessToken, accessToken, "Token.AccessToken mismatch")
	assertEqual(t, got.AccessTokenHash, hashToken(accessToken), "Token.AccessTokenHashed mismatch")
	// We don't care what the encrypted token is here. The fact that we store encrypted values is an
	// implementation detail; the rest of the program doesn't care.
	assertEqual(t, got.UserID, userID, "Token.UserID mismatch")
	assertEqual(t, got.DeviceID, deviceID, "Token.DeviceID mismatch")
	assertEqualTimes(t, got.LastSeen, lastSeen, "Token.LastSeen mismatch")
}

func assertEqual(t *testing.T, got, want, msg string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %s want %s", msg, got, want)
	}
}

func assertEqualTimes(t *testing.T, got, want time.Time, msg string) {
	t.Helper()
	// Postgres stores timestamps with microsecond resolution, so we might lose some
	// precision by storing and fetching a time.Time in/from the DB. Resolution of
	// a second will suffice.
	if !got.Round(time.Second).Equal(want.Round(time.Second)) {
		t.Fatalf("%s: got %v want %v", msg, got, want)
	}
}

// see devices_table_test.go for tests which join the tokens and devices tables.
