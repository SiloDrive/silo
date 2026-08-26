package utils

import (
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/option"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func IsValidUUID(u string) bool {
	_, err := uuid.Parse(u)
	return err == nil
}

// AudNotif is the audience on the one JWT Silo still issues.
//
// There were two, and the pair was the point: every Silo JWT is signed with
// the same key (option.JWTPrivateKey), so a token of one kind parses cleanly
// as another kind's claims, and an audience named the only validator allowed
// to accept it. The session JWT is gone -- a session is a Credential row now
// -- so one audience is left, and it is still required rather than assumed:
// the notification server verifies it in a process with no database, where the
// audience is the only thing distinguishing this token from any other the key
// could sign.
const AudNotif = "silo:notif"

// SigningAlg is the only JWT algorithm Silo issues or accepts. Validators pass
// it to jwt.WithValidMethods so a token cannot select its own algorithm.
const SigningAlg = "HS256"

type MyClaims struct {
	LibraryID string `json:"library_id"`
	UserName  string `json:"username"`
	jwt.RegisteredClaims
}

func GenNotifJWTToken(libraryID, user string, exp int64) (string, error) {
	claims := new(MyClaims)
	claims.ExpiresAt = jwt.NewNumericDate(time.Unix(exp, 0))
	claims.Audience = jwt.ClaimStrings{AudNotif}
	claims.LibraryID = libraryID
	claims.UserName = user

	token := jwt.NewWithClaims(jwt.GetSigningMethod(SigningAlg), claims)
	tokenString, err := token.SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		err := fmt.Errorf("failed to gen jwt token for library %s: %w", libraryID, err)
		return "", err
	}

	return tokenString, nil
}
