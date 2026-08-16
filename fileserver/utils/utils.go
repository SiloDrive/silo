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

func IsObjectIDValid(objID string) bool {
	if len(objID) != 40 {
		return false
	}
	for i := 0; i < len(objID); i++ {
		c := objID[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// Every Silo JWT is signed with the same key (option.JWTPrivateKey), so a
// token of one kind parses cleanly as another kind's claims — a notification
// token read as session claims yields an empty email rather than an error.
// Each token Silo issues *and validates* therefore carries an audience naming
// the only validator allowed to accept it, and each validator requires it.
const (
	AudSession = "silo:session"
	AudNotif   = "silo:notif"
)

// SigningAlg is the only JWT algorithm Silo issues or accepts. Validators pass
// it to jwt.WithValidMethods so a token cannot select its own algorithm.
const SigningAlg = "HS256"

// SeahubClaims is deliberately left without an audience: unlike the session
// and notification tokens, this one is consumed by Seahub rather than by Silo,
// and PyJWT rejects a token carrying an `aud` it wasn't told to expect.
type SeahubClaims struct {
	IsInternal bool `json:"is_internal"`
	jwt.RegisteredClaims
}

func GenSeahubJWTToken() (string, error) {
	claims := new(SeahubClaims)
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Second * 300))
	claims.IsInternal = true

	token := jwt.NewWithClaims(jwt.GetSigningMethod("HS256"), claims)
	tokenString, err := token.SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		err := fmt.Errorf("failed to gen seahub jwt token: %w", err)
		return "", err
	}

	return tokenString, nil
}

type MyClaims struct {
	RepoID   string `json:"repo_id"`
	UserName string `json:"username"`
	jwt.RegisteredClaims
}

func GenNotifJWTToken(repoID, user string, exp int64) (string, error) {
	claims := new(MyClaims)
	claims.ExpiresAt = jwt.NewNumericDate(time.Unix(exp, 0))
	claims.Audience = jwt.ClaimStrings{AudNotif}
	claims.RepoID = repoID
	claims.UserName = user

	token := jwt.NewWithClaims(jwt.GetSigningMethod(SigningAlg), claims)
	tokenString, err := token.SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		err := fmt.Errorf("failed to gen jwt token for repo %s: %w", repoID, err)
		return "", err
	}

	return tokenString, nil
}
