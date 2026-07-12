package call

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"
)

func NewID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "call-" + hex.EncodeToString(random[:]), nil
}

type State string

const (
	Created       State = "created"
	Submitted     State = "submitted"
	Working       State = "working"
	InputRequired State = "input_required"
	AuthRequired  State = "auth_required"
	Completed     State = "completed"
	Failed        State = "failed"
	Canceled      State = "canceled"
	Rejected      State = "rejected"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var safeErrorCode = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var knownStates = map[State]bool{
	Created: true, Submitted: true, Working: true, InputRequired: true,
	AuthRequired: true, Completed: true, Failed: true, Canceled: true, Rejected: true,
}

type Call struct {
	ID           string    `json:"id"`
	CapabilityID string    `json:"capability_id"`
	IdentityID   string    `json:"identity_id,omitempty"`
	State        State     `json:"state"`
	Code         string    `json:"code,omitempty"`
	Message      string    `json:"message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

var transitions = map[State]map[State]bool{
	Created:       {Submitted: true, Failed: true, Rejected: true, Canceled: true},
	Submitted:     {Working: true, Failed: true, Canceled: true, Rejected: true},
	Working:       {InputRequired: true, AuthRequired: true, Completed: true, Failed: true, Canceled: true, Rejected: true},
	InputRequired: {Working: true, Canceled: true, Failed: true},
	AuthRequired:  {Working: true, Canceled: true, Failed: true},
}

func CanTransition(from, to State) bool { return transitions[from][to] }

func validateErrorMetadata(code, message string) error {
	if (code != "" && !safeErrorCode.MatchString(code)) || len(message) > 1024 || !utf8.ValidString(message) {
		return errors.New("call error metadata exceeds limit")
	}
	for _, r := range message {
		if unicode.IsControl(r) {
			return errors.New("call error metadata contains control characters")
		}
	}
	return nil
}

func (c Call) Validate() error {
	if !safeID.MatchString(c.ID) {
		return errors.New("invalid call ID")
	}
	if c.CapabilityID == "" || c.IdentityID == "" || !knownStates[c.State] {
		return errors.New("call is incomplete")
	}
	return validateErrorMetadata(c.Code, c.Message)
}
