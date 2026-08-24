package silod

// What the server says happened, for an operator reading the log.
//
// These are observations rather than a protocol: nothing downstream parses
// them, and a client never sees one. They exist because a head that moved and
// bytes that arrived are the two things you want in the log when a sync goes
// wrong.

import (
	log "github.com/sirupsen/logrus"
)

type statsEventData struct {
	eType     string
	user      string
	libraryID string
	bytes     uint64
}

func sendStatisticMsg(libraryID, user, operation string, bytes uint64) {
	rData := &statsEventData{operation, user, libraryID, bytes}

	publishStatsEvent(rData)
}

func publishStatsEvent(rData *statsEventData) {
	log.Infof("stats event: type=%s user=%s library=%s bytes=%d",
		rData.eType, rData.user, rData.libraryID, rData.bytes)
}

func publishUpdateEvent(libraryID string, commitID string) {
	log.Infof("update event: library=%s commit=%s", libraryID, commitID)
}
