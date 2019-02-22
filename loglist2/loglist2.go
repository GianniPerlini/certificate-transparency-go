// Copyright 2018 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package loglist2 allows parsing and searching of the master CT Log list.
// It expects the log list to conform to the v2beta schema.
package loglist2

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/certificate-transparency-go/tls"
)

const (
	// LogListURL has the master URL for Google Chrome's log list.
	LogListURL = "https://www.gstatic.com/ct/log_list/v2beta/log_list.json"
	// LogListSignatureURL has the URL for the signature over Google Chrome's log list.
	LogListSignatureURL = "https://www.gstatic.com/ct/log_list/v2beta/log_list.sig"
	// AllLogListURL has the URL for the list of all known logs (which isn't signed).
	AllLogListURL = "https://www.gstatic.com/ct/log_list/v2beta/all_logs_list.json"
)

// Manually mapped from https://www.gstatic.com/ct/log_list/v2beta/log_list_schema.json

// LogList holds a collection of CT logs, grouped by operator.
type LogList struct {
	// Operators is a list of CT log operators and and the logs they operate.
	Operators []*Operator `json:"operators"`
}

// Operator holds a collection of CT logs run by the same organisation.
// It also provides information about that organisation, e.g. contact details.
type Operator struct {
	// Name is the name of the CT log operator.
	Name string `json:"name"`
	// Email lists the email addresses that can be used to contact this log
	// operator.
	Email []string `json:"email"`
	// Logs is a list of CT logs run by this operator.
	Logs []*Log `json:"logs"`
}

// Log describes a single CT log.
type Log struct {
	// Description is a human-readable string that describes the log.
	Description string `json:"description,omitempty"`
	// LogID is the SHA-256 hash of the log's public key.
	LogID []byte `json:"log_id"`
	// Key is the public key with which signatures can be verified.
	Key []byte `json:"key"`
	// URL is the address of the HTTPS API.
	URL string `json:"url"`
	// DNS is the address of the DNS API.
	DNS string `json:"dns,omitempty"`
	// MMD is the Maximum Merge Delay, in seconds. All submitted
	// certificates must be incorporated into the log within this time.
	MMD int32 `json:"mmd"`
	// State is the current state of the log, from the perspective of the
	// log list distributor.
	State *LogStates `json:"state,omitempty"`
	// TemporalInterval, if set, indicates that this log only accepts
	// certificates with a NotBefore date in this time range.
	TemporalInterval *TemporalInterval `json:"temporal_interval,omitempty"`
	// Type indicates the purpose of this log, e.g. "test" or "prod".
	Type string `json:"log_type,omitempty"`
}

// TemporalInterval is a time range.
type TemporalInterval struct {
	// StartInclusive is the beginning of the time range.
	StartInclusive time.Time `json:"start_inclusive"`
	// EndExclusive is just after the end of the time range.
	EndExclusive time.Time `json:"end_exclusive"`
}

// LogStatus indicates Log status.
type LogStatus int

// LogStatus values
const (
	UndefinedLogStatus LogStatus = iota
	PendingLogStatus
	QualifiedLogStatus
	UsableLogStatus
	FrozenLogStatus
	RetiredLogStatus
	RejectedLogStatus
)

//go:generate stringer -type=LogStatus

// LogStates are the states that a CT log can be in, from the perspective of a
// user agent. Only one should be set - this is the current state.
type LogStates struct {
	// Pending indicates that the log is in the "pending" state.
	Pending *LogState `json:"pending,omitempty"`
	// Qualified indicates that the log is in the "qualified" state.
	Qualified *LogState `json:"qualified,omitempty"`
	// Usable indicates that the log is in the "usable" state.
	Usable *LogState `json:"usable,omitempty"`
	// Frozen indicates that the log is in the "frozen" state.
	Frozen *FrozenLogState `json:"frozen,omitempty"`
	// Retired indicates that the log is in the "retired" state.
	Retired *LogState `json:"retired,omitempty"`
	// Rejected indicates that the log is in the "rejected" state.
	Rejected *LogState `json:"rejected,omitempty"`
}

// LogState contains details on the current state of a CT log.
type LogState struct {
	// Timestamp is the time when the state began.
	Timestamp time.Time `json:"timestamp"`
}

// FrozenLogState contains details on the current state of a frozen CT log.
type FrozenLogState struct {
	LogState
	// FinalTreeHead is the root hash and tree size that the CT log was
	// frozen at. This should never change while the log is frozen.
	FinalTreeHead TreeHead `json:"final_tree_head"`
}

// TreeHead is the root hash and tree size of a CT log.
type TreeHead struct {
	// SHA256RootHash is the root hash of the CT log's Merkle tree.
	SHA256RootHash []byte `json:"sha256_root_hash"`
	// TreeSize is the size of the CT log's Merkle tree.
	TreeSize int64 `json:"tree_size"`
}

// LogStatus method returns Log-status enum value for descriptive struct.
func (ls *LogStates) LogStatus() LogStatus {
	switch {
	case ls == nil:
		return UndefinedLogStatus
	case ls.Pending != nil:
		return PendingLogStatus
	case ls.Qualified != nil:
		return QualifiedLogStatus
	case ls.Usable != nil:
		return UsableLogStatus
	case ls.Frozen != nil:
		return FrozenLogStatus
	case ls.Retired != nil:
		return RetiredLogStatus
	case ls.Rejected != nil:
		return RejectedLogStatus
	default:
		return UndefinedLogStatus
	}
}

// String method returns printable name of the state.
func (ls *LogStates) String() string {
	return ls.LogStatus().String()
}

// Active picks the set-up state. If multiple states are set (not expected) picks one of them.
func (ls *LogStates) Active() (*LogState, *FrozenLogState) {
	if ls == nil {
		return nil, nil
	}
	switch {
	case ls.Pending != nil:
		return ls.Pending, nil
	case ls.Qualified != nil:
		return ls.Qualified, nil
	case ls.Usable != nil:
		return ls.Usable, nil
	case ls.Frozen != nil:
		return nil, ls.Frozen
	case ls.Retired != nil:
		return ls.Retired, nil
	case ls.Rejected != nil:
		return ls.Rejected, nil
	default:
		return nil, nil
	}
}

// NewFromJSON creates a LogList from JSON encoded data.
func NewFromJSON(llData []byte) (*LogList, error) {
	var ll LogList
	if err := json.Unmarshal(llData, &ll); err != nil {
		return nil, fmt.Errorf("failed to parse log list: %v", err)
	}
	return &ll, nil
}

// NewFromSignedJSON creates a LogList from JSON encoded data, checking a
// signature along the way. The signature data should be provided as the
// raw signature data.
func NewFromSignedJSON(llData, rawSig []byte, pubKey crypto.PublicKey) (*LogList, error) {
	sigAlgo := tls.Anonymous
	switch pkType := pubKey.(type) {
	case *rsa.PublicKey:
		sigAlgo = tls.RSA
	case *ecdsa.PublicKey:
		sigAlgo = tls.ECDSA
	default:
		return nil, fmt.Errorf("Unsupported public key type %v", pkType)
	}
	tlsSig := tls.DigitallySigned{
		Algorithm: tls.SignatureAndHashAlgorithm{
			Hash:      tls.SHA256,
			Signature: sigAlgo,
		},
		Signature: rawSig,
	}
	if err := tls.VerifySignature(pubKey, llData, tlsSig); err != nil {
		return nil, fmt.Errorf("failed to verify signature: %v", err)
	}
	return NewFromJSON(llData)
}

// FindLogByName returns all logs whose names contain the given string.
func (ll *LogList) FindLogByName(name string) []*Log {
	name = strings.ToLower(name)
	var results []*Log
	for _, op := range ll.Operators {
		for _, log := range op.Logs {
			if strings.Contains(strings.ToLower(log.Description), name) {
				results = append(results, log)
			}
		}
	}
	return results
}

// FindLogByURL finds the log with the given URL.
func (ll *LogList) FindLogByURL(url string) *Log {
	for _, op := range ll.Operators {
		for _, log := range op.Logs {
			// Don't count trailing slashes
			if strings.TrimRight(log.URL, "/") == strings.TrimRight(url, "/") {
				return log
			}
		}
	}
	return nil
}

// FindLogByKeyHash finds the log with the given key hash.
func (ll *LogList) FindLogByKeyHash(keyhash [sha256.Size]byte) *Log {
	for _, op := range ll.Operators {
		for _, log := range op.Logs {
			if bytes.Equal(log.LogID, keyhash[:]) {
				return log
			}
		}
	}
	return nil
}

// FindLogByKeyHashPrefix finds all logs whose key hash starts with the prefix.
func (ll *LogList) FindLogByKeyHashPrefix(prefix string) []*Log {
	var results []*Log
	for _, op := range ll.Operators {
		for _, log := range op.Logs {
			hh := hex.EncodeToString(log.LogID[:])
			if strings.HasPrefix(hh, prefix) {
				results = append(results, log)
			}
		}
	}
	return results
}

// FindLogByKey finds the log with the given DER-encoded key.
func (ll *LogList) FindLogByKey(key []byte) *Log {
	for _, op := range ll.Operators {
		for _, log := range op.Logs {
			if bytes.Equal(log.Key[:], key) {
				return log
			}
		}
	}
	return nil
}

var hexDigits = regexp.MustCompile("^[0-9a-fA-F]+$")

// FuzzyFindLog tries to find logs that match the given unspecified input,
// whose format is unspecified.  This generally returns a single log, but
// if text input that matches multiple log descriptions is provided, then
// multiple logs may be returned.
func (ll *LogList) FuzzyFindLog(input string) []*Log {
	input = strings.Trim(input, " \t")
	if logs := ll.FindLogByName(input); len(logs) > 0 {
		return logs
	}
	if log := ll.FindLogByURL(input); log != nil {
		return []*Log{log}
	}
	// Try assuming the input is binary data of some form.  First base64:
	if data, err := base64.StdEncoding.DecodeString(input); err == nil {
		if len(data) == sha256.Size {
			var hash [sha256.Size]byte
			copy(hash[:], data)
			if log := ll.FindLogByKeyHash(hash); log != nil {
				return []*Log{log}
			}
		}
		if log := ll.FindLogByKey(data); log != nil {
			return []*Log{log}
		}
	}
	// Now hex, but strip all internal whitespace first.
	input = stripInternalSpace(input)
	if data, err := hex.DecodeString(input); err == nil {
		if len(data) == sha256.Size {
			var hash [sha256.Size]byte
			copy(hash[:], data)
			if log := ll.FindLogByKeyHash(hash); log != nil {
				return []*Log{log}
			}
		}
		if log := ll.FindLogByKey(data); log != nil {
			return []*Log{log}
		}
	}
	// Finally, allow hex strings with an odd number of digits.
	if hexDigits.MatchString(input) {
		if logs := ll.FindLogByKeyHashPrefix(input); len(logs) > 0 {
			return logs
		}
	}

	return nil
}

func stripInternalSpace(input string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsSpace(r) {
			return r
		}
		return -1
	}, input)
}
