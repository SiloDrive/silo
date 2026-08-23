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
	eType  string
	user   string
	repoID string
	bytes  uint64
}

func sendStatisticMsg(repoID, user, operation string, bytes uint64) {
	rData := &statsEventData{operation, user, repoID, bytes}

	publishStatsEvent(rData)
}

func publishStatsEvent(rData *statsEventData) {
	log.Infof("stats event: type=%s user=%s repo=%s bytes=%d",
		rData.eType, rData.user, rData.repoID, rData.bytes)
}

func publishUpdateEvent(repoID string, commitID string) {
	log.Infof("update event: repo=%s commit=%s", repoID, commitID)
}
