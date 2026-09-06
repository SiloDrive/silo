// The storage knobs an operator is allowed to move, and the invariants that
// keep a set of them from being nonsense.
//
// These live behind a struct rather than being exported variables because two
// of them are read once, when a packStore is created, and a value written
// after that has no effect at all -- see packMaxAge. A setter that must be
// called before the first library opens is a thing a doc comment can say and a
// caller can get right; three exported variables with the same requirement is
// not.
//
// objstore does not read the config file. It is given numbers by whoever did,
// which keeps this package testable without one and keeps the option package
// out of the read path.
package objstore

import (
	"fmt"
	"time"
)

// Tuning is the pack writer's behaviour, as an operator may set it.
//
// Zero is not a valid Tuning: every field has to be positive, and there is no
// "unset means keep the current value" reading. A caller that wants to change
// one knob starts from DefaultTuning and edits it, so that what is applied is
// always a complete, checked set rather than whatever the previous call left
// behind.
type Tuning struct {
	// PackTarget is the size at which a pack is full enough to seal.
	PackTarget int64
	// PackMaxAge is how long a pack may hold its oldest frame before it is
	// sealed regardless of size.
	PackMaxAge time.Duration
	// PackSweep is how often the age sealer looks.
	PackSweep time.Duration
}

// DefaultTuning is what Silo runs on when nobody has configured anything.
//
// It reads the constants rather than restating their values, so that each
// default lives in one place, beside the reasoning for it. The constants and
// not the package variables: a test that shortened a variable has changed the
// store under test, not what Silo runs on by default.
func DefaultTuning() Tuning {
	return Tuning{
		PackTarget: defaultPackTarget,
		PackMaxAge: defaultPackMaxAge,
		PackSweep:  defaultPackSweep,
	}
}

// minPackTarget is the smallest pack size an operator may configure.
//
// Tests set packTarget far below this -- a pack that seals after three frames
// is how sealing gets tested without writing half a gigabyte -- and they do it
// by assigning the variable, which is deliberately still possible. This floor
// is on the configured path only, where the number came from a human and a
// missing unit ("pack_size = 512" meaning bytes) would otherwise produce a
// store that seals a pack per object and never says why.
const minPackTarget = 1 << 20

// maxPackTarget is a ceiling on the same value. A pack's whole index and bloom
// filter are held in memory once it is sealed, and a rewrite copies up to one
// pack before it can publish anything, so an enormous target is not a faster
// store -- it is a longer uninterruptible copy and a larger resident index.
const maxPackTarget = 64 << 30

// Validate reports whether a set of knobs can be applied together.
//
// The sweep interval is checked against the age rather than on its own,
// because it is not independently meaningful: the age rule promises a pack is
// sealed within about PackMaxAge, and a sweeper that looks less often than
// that cannot keep the promise. A sweep of an hour against an age of five
// minutes is not a slow sweep, it is an age rule that silently does not hold.
func (t Tuning) Validate() error {
	if t.PackTarget < minPackTarget || t.PackTarget > maxPackTarget {
		return fmt.Errorf("pack size must be between %d and %d bytes, got %d",
			minPackTarget, maxPackTarget, t.PackTarget)
	}
	if t.PackMaxAge <= 0 {
		return fmt.Errorf("pack max age must be positive, got %s", t.PackMaxAge)
	}
	if t.PackSweep <= 0 {
		return fmt.Errorf("pack sweep interval must be positive, got %s", t.PackSweep)
	}
	if t.PackSweep > t.PackMaxAge {
		return fmt.Errorf("pack sweep interval (%s) must not exceed pack max age (%s), "+
			"or a pack is not sealed within the age it promises", t.PackSweep, t.PackMaxAge)
	}
	return nil
}

// Configure applies a set of knobs to this process.
//
// **Call it before the first library is opened.** PackMaxAge and PackSweep are
// copied when a packStore is created, which happens on the first write to any
// library, and a value set after that is read by nothing. PackTarget is read on
// every append and so does take effect late -- but a Tuning that is half in
// force is worse than one that is not, so the whole thing has one rule.
//
// It validates before it assigns, so a rejected Tuning leaves the process on
// the values it already had rather than on the first two fields of a bad set.
func Configure(t Tuning) error {
	if err := t.Validate(); err != nil {
		return err
	}
	packTarget = t.PackTarget
	packMaxAge = t.PackMaxAge
	packSweep = t.PackSweep
	return nil
}
