package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
)

const Phase3ReceiptArtifactType = "phase-3-action-receipt"

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// DigestEventData returns the canonical digest of the inline action output.
// Receipt control fields are added only after this digest is calculated.
func DigestEventData(data EventData) (string, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal action output: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(payload)), nil
}

func ExpectedActionArtifactID(actionID string) string {
	return "action-" + actionID
}

// ValidateManagedReceiptEvidence enforces the narrow phase-3 evidence
// contract. ApplyPatch is the only action with an Artifact side effect; its
// inline output and Artifact bytes have independent digests.
func ValidateManagedReceiptEvidence(
	actionType CommandType,
	actionID string,
	plannedAttemptID string,
	output EventData,
	outputDigest string,
	artifactID string,
	artifactDigest string,
) error {
	if err := ValidateManagedActionOutput(actionType, plannedAttemptID, output); err != nil {
		return err
	}
	expectedOutputDigest, err := DigestEventData(output)
	if err != nil {
		return err
	}
	if outputDigest == "" || outputDigest != expectedOutputDigest {
		return fmt.Errorf(
			"%w: inline output digest %q does not match %q",
			ErrInvalidEvent,
			outputDigest,
			expectedOutputDigest,
		)
	}
	if actionType == CommandApplyPatch {
		expectedArtifactID := ExpectedActionArtifactID(actionID)
		if artifactID == "" || artifactID != expectedArtifactID {
			return fmt.Errorf(
				"%w: ApplyPatch Artifact ID %q does not match %q",
				ErrInvalidEvent,
				artifactID,
				expectedArtifactID,
			)
		}
		if !sha256DigestPattern.MatchString(artifactDigest) {
			return fmt.Errorf(
				"%w: ApplyPatch Artifact digest %q is not SHA-256",
				ErrInvalidEvent,
				artifactDigest,
			)
		}
		return nil
	}
	if artifactID != "" || artifactDigest != "" {
		return fmt.Errorf(
			"%w: action %s must not carry phase-3 Artifact evidence",
			ErrInvalidEvent,
			actionType,
		)
	}
	return nil
}
