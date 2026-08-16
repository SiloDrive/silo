package authmgr

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	jwt "github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

var readDB *sql.DB
var writeDB *sql.DB

func Init(ccnetReadDB, ccnetWriteDB *sql.DB) {
	readDB = ccnetReadDB
	writeDB = ccnetWriteDB
}

// Legacy fixed salt used by old Seafile SHA256 password hashing.
var legacySalt = []byte{0xdb, 0x91, 0x45, 0xc3, 0x06, 0xc7, 0xcc, 0x26}

// ValidatePassword checks email/password against the EmailUser table.
// Returns the user email (possibly lowercased) on success.
func ValidatePassword(email, password string) (string, error) {
	if password == "!" {
		return "", fmt.Errorf("invalid password")
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	var storedPasswd string
	row := readDB.QueryRowContext(ctx, "SELECT passwd FROM EmailUser WHERE email=?", email)
	err := row.Scan(&storedPasswd)
	if err == sql.ErrNoRows {
		emailDown := strings.ToLower(email)
		row = readDB.QueryRowContext(ctx, "SELECT passwd FROM EmailUser WHERE email=?", emailDown)
		err = row.Scan(&storedPasswd)
		if err != nil {
			return "", fmt.Errorf("user not found")
		}
		email = emailDown
	} else if err != nil {
		return "", fmt.Errorf("database error: %v", err)
	}

	if !validatePasswd(password, storedPasswd) {
		return "", fmt.Errorf("incorrect password")
	}

	return email, nil
}

func validatePasswd(password, storedPasswd string) bool {
	if storedPasswd == "!" {
		return false
	}

	// Check for known prefix before falling back to length-based dispatch
	if strings.HasPrefix(storedPasswd, "PBKDF2SHA256$") {
		return validatePBKDF2SHA256(password, storedPasswd)
	}

	hashLen := len(storedPasswd)
	switch hashLen {
	case sha256.Size * 2:
		return validateSHA256Salted(password, storedPasswd)
	case sha1.Size * 2:
		return validateSHA1(password, storedPasswd)
	default:
		return false
	}
}

func validatePBKDF2SHA256(password, storedPasswd string) bool {
	parts := strings.Split(storedPasswd, "$")
	if len(parts) != 4 {
		return false
	}

	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}

	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}

	expectedHash := parts[3]

	derived := pbkdf2.Key([]byte(password), salt, iter, sha256.Size, sha256.New)
	computedHash := hex.EncodeToString(derived)

	return subtle.ConstantTimeCompare([]byte(computedHash), []byte(expectedHash)) == 1
}

func validateSHA256Salted(password, storedPasswd string) bool {
	h := sha256.New()
	h.Write([]byte(password))
	h.Write(legacySalt)
	computed := hex.EncodeToString(h.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedPasswd)) == 1
}

func validateSHA1(password, storedPasswd string) bool {
	h := sha1.New()
	h.Write([]byte(password))
	computed := hex.EncodeToString(h.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedPasswd)) == 1
}

type SessionClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

func GenerateSessionToken(email string) (string, error) {
	if email == "" {
		return "", fmt.Errorf("refusing to issue a session token with no email")
	}

	now := time.Now()
	claims := SessionClaims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudSession},
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		return "", fmt.Errorf("failed to sign session token: %v", err)
	}

	return tokenString, nil
}

func ValidateSessionToken(tokenString string) (string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &SessionClaims{},
		func(token *jwt.Token) (interface{}, error) {
			return []byte(option.JWTPrivateKey), nil
		},
		// The notification tokens are signed with this same key, so the
		// signature alone proves nothing about which validator a token was
		// meant for. WithAudience makes that explicit and rejects a token
		// carrying no audience at all.
		jwt.WithValidMethods([]string{utils.SigningAlg}),
		jwt.WithAudience(utils.AudSession),
	)
	if err != nil {
		return "", fmt.Errorf("invalid token: %v", err)
	}

	claims, ok := token.Claims.(*SessionClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid token claims")
	}

	// A token of another kind that somehow satisfied the checks above would
	// carry no email, and an empty identity must never reach a handler:
	// share.CheckPerm("") denies, but repo creation would happily accept it.
	if claims.Email == "" {
		return "", fmt.Errorf("token has no email claim")
	}

	return claims.Email, nil
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("failed to generate salt: %v", err)
	}
	iterations := 10000
	derived := pbkdf2.Key([]byte(password), salt, iterations, sha256.Size, sha256.New)
	return fmt.Sprintf("PBKDF2SHA256$%d$%s$%s",
		iterations, hex.EncodeToString(salt), hex.EncodeToString(derived)), nil
}

// EnsureAdmin creates an admin user if it doesn't already exist.
// Uses INSERT OR IGNORE to avoid TOCTOU races.
func EnsureAdmin(email, password string) error {
	if email == "" || password == "" {
		return nil
	}

	hash, err := hashPassword(password)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	sqlStr := dbutil.InsertOrIgnore("EmailUser", "email, passwd, is_staff, is_active, ctime")
	result, err := writeDB.ExecContext(ctx, sqlStr, email, hash, 1, 1, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("failed to create admin user: %v", err)
	}

	rows, _ := result.RowsAffected()
	if rows > 0 {
		log.Infof("Created admin user: %s", email)
	} else {
		log.Infof("Admin user %s already exists", email)
	}
	return nil
}
