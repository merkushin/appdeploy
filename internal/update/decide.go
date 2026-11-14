package update

import (
	"fmt"
	"sort"
)

// PeerState is what a server knows of another one when deciding whether it is
// its turn to update.
type PeerState struct {
	Reachable bool
	Version   string
	Busy      bool // deploying something right now
}

// Decision is the outcome of Decide.
type Decision struct {
	Act    bool   // install the desired version now
	Done   bool   // nothing to do: already there (or no version is wanted)
	Reason string // why not, when neither Act nor Done
}

// Decide says whether this server should install the desired version now.
// Servers update one at a time in name order, so that the first one is a canary
// and a failure stops the rest: a server waits until every reachable server
// before it runs the desired version, and until nobody is deploying. An
// unreachable server is not waited for.
func Decide(desired, current, self string, servers []string, peers map[string]PeerState, localBusy bool, failedTo string) Decision {
	if desired == "" {
		return Decision{Done: true}
	}
	dv, ok := ParseVersion(desired)
	if !ok {
		return Decision{Reason: fmt.Sprintf("the desired version %q is not a version like v1.2.3", desired)}
	}
	cv, ok := ParseVersion(current)
	if !ok {
		return Decision{Reason: fmt.Sprintf("this is a %q build; only released builds are managed", current)}
	}
	if !Newer(dv, cv) {
		return Decision{Done: true}
	}
	if failedTo != "" && norm(failedTo) == norm(desired) {
		return Decision{Reason: fmt.Sprintf("installing %s failed here before and was reverted; ask for another version to try again", desired)}
	}
	if localBusy {
		return Decision{Reason: "waiting for the deploys running here to finish"}
	}
	order := append([]string(nil), servers...)
	sort.Strings(order)
	for _, name := range order {
		if name == self {
			continue
		}
		if p := peers[name]; p.Reachable && p.Busy {
			return Decision{Reason: fmt.Sprintf("waiting for %s to finish a deploy", name)}
		}
	}
	for _, name := range order {
		if name == self {
			break
		}
		p := peers[name]
		if !p.Reachable {
			continue
		}
		pv, ok := ParseVersion(p.Version)
		if !ok || Newer(dv, pv) {
			return Decision{Reason: fmt.Sprintf("waiting for %s to update first (it runs %s)", name, p.Version)}
		}
	}
	return Decision{Act: true}
}
