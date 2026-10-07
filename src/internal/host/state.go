package host

import (
	"errors"
	"io"
	"time"

	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/github"
)

type AdmissionPolicy string

const (
	PolicyRun  AdmissionPolicy = "run"
	PolicyHold AdmissionPolicy = "hold"
)

type ControlIntent struct {
	Policy AdmissionPolicy `json:"policy"`
}
type DrainIntent struct {
	Requested bool   `json:"requested"`
	Reason    string `json:"reason"`
}
type Admission struct {
	config.RuntimeSnapshot
	ID, HostID, ProfileID, ContainerName, ContainerID, RunnerName string
	Auth                                                          github.AuthIdentity
	ScopeDigest                                                   string
	Candidate                                                     github.JobRef
	Actual                                                        *github.JobRef
	RunnerID                                                      int64
	Phase                                                         string
	StartTerminal                                                 bool
	Drain                                                         DrainIntent `json:"drain"`
}
type PhysicalObjectRef struct{ DaemonID, Endpoint, ObjectID, Kind, Name string }
type PhysicalProof struct {
	HostID, AdmissionID, ConfigDigest, InventoryDigest                              string
	ProvenAt                                                                        time.Time
	RequestsTerminal, InventoryComplete, NoLive, NoRevival, Destroyed, NeverCreated bool
	Objects                                                                         []PhysicalObjectRef
	EvidenceIDs                                                                     []string
	Digest                                                                          string
}
type ReleaseRecord struct {
	Path          string
	Physical      PhysicalProof
	JobConclusion string
}
type ReleaseSummary struct {
	AdmissionID, Path, PhysicalDigest, JobConclusion string
	ReleasedAt                                       time.Time
}
type Debt struct {
	Admission     Admission
	Release       ReleaseRecord
	Reason, Scope string
	Next          time.Time
}
type RequestID struct {
	HostID string `json:"host_id"`
	Seq    uint64 `json:"seq"`
}
type Request struct {
	Version   int                          `json:"version"`
	ID        RequestID                    `json:"id"`
	Action    string                       `json:"action"`
	ProfileID string                       `json:"profile_id,omitempty"`
	Binding   *config.ParticipationBinding `json:"binding,omitempty"`
}
type Receipt struct {
	ID     RequestID `json:"id"`
	Digest string    `json:"digest"`
	Result string    `json:"result"`
}
type ControlJournal struct {
	HighWater uint64   `json:"high_water"`
	Last      *Receipt `json:"last"`
	Applying  *Request `json:"applying"`
}
type HostState struct {
	Schema       int             `json:"schema_version"`
	HostID       string          `json:"host_id"`
	Phase        string          `json:"phase"`
	Cursor       string          `json:"cursor"`
	Control      ControlIntent   `json:"control"`
	Journal      ControlJournal  `json:"journal"`
	Active       *Admission      `json:"active"`
	LastRelease  *ReleaseRecord  `json:"last_release"`
	LastReleased *ReleaseSummary `json:"last_released"`
	Debts        []Debt          `json:"debts"`
}
type RequestDisposition string

const (
	RequestAccept  RequestDisposition = "accept"
	RequestReplay  RequestDisposition = "replay"
	RequestRecover RequestDisposition = "recover"
)

var (
	ErrCorruptState       = errors.New("CORRUPT_STATE")
	ErrHostMismatch       = errors.New("HOST_ID_MISMATCH")
	ErrStaleRequest       = errors.New("STALE_REQUEST")
	ErrRequestConflict    = errors.New("REQUEST_CONFLICT")
	ErrOutOfOrder         = errors.New("OUT_OF_ORDER")
	ErrSequenceExhausted  = errors.New("SEQUENCE_EXHAUSTED")
	ErrActivationRequired = errors.New("ACTIVATION_REQUIRED")
	ErrStateLostBlocked   = errors.New("STATE_LOST_BLOCKED")
	ErrUnsupportedHost    = errors.New("UNSUPPORTED_HOST")
)

type StateStore interface {
	Load() (HostState, error)
	Save(HostState) error
	TakeRequest() (*Request, error)
	CleanupRequest(Request) error
}
type Locker interface{ Acquire() (io.Closer, error) }

func InitialState(hostID string) HostState {
	return HostState{Schema: 1, HostID: hostID, Phase: "reconciling", Control: ControlIntent{Policy: PolicyHold}}
}

func SummarizeRelease(record ReleaseRecord) ReleaseSummary {
	return ReleaseSummary{AdmissionID: record.Physical.AdmissionID, Path: record.Path, PhysicalDigest: record.Physical.Digest, JobConclusion: record.JobConclusion, ReleasedAt: record.Physical.ProvenAt}
}
