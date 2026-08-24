package tokenstore

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	TokenExpireTime = 3600 // 1 hour
	CleanupInterval = 5 * time.Minute
)

type AccessInfo struct {
	LibraryID  string
	ObjID      string
	Op         string
	User       string
	ExpireTime int64
	OneTime    bool
}

var tokens sync.Map

func CreateToken(libraryID, objID, op, user string, oneTime bool) string {
	token := uuid.New().String()
	info := &AccessInfo{
		LibraryID:  libraryID,
		ObjID:      objID,
		Op:         op,
		User:       user,
		ExpireTime: time.Now().Unix() + TokenExpireTime,
		OneTime:    oneTime,
	}
	tokens.Store(token, info)
	return token
}

// QueryToken returns the access a token grants, redeeming it if it is
// one-time.
//
// The redemption is a single LoadAndDelete rather than a Load followed by a
// Delete. Under the old pair, two requests arriving together both saw the
// token before either removed it, and both were served — a one-time token,
// which exists precisely so that a URL carrying it cannot be replayed, could
// be spent twice.
func QueryToken(token string) *AccessInfo {
	val, ok := tokens.Load(token)
	if !ok {
		return nil
	}
	info := val.(*AccessInfo)

	if info.OneTime {
		// Exactly one caller gets loaded=true; anyone racing it gets nothing.
		claimed, loaded := tokens.LoadAndDelete(token)
		if !loaded {
			return nil
		}
		info = claimed.(*AccessInfo)
	}

	if time.Now().Unix() >= info.ExpireTime {
		tokens.Delete(token)
		return nil
	}

	return info
}

func DeleteToken(token string) {
	tokens.Delete(token)
}

func StartCleanup() {
	go func() {
		ticker := time.NewTicker(CleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now().Unix()
			tokens.Range(func(key, value interface{}) bool {
				info := value.(*AccessInfo)
				if now >= info.ExpireTime {
					tokens.Delete(key)
				}
				return true
			})
		}
	}()
}
