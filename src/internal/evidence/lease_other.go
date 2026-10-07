//go:build !linux

package evidence

// AcquireHostLease has no production implementation off Linux; the lease file
// mechanism is still exercised through the in-package test seam.
func AcquireHostLease() (*Lease, error) { return nil, ErrUnsupportedEvidenceHost }

// productionNamespaceCheck is unavailable off Linux; the production reader
// fixed root must not be used.
func productionNamespaceCheck() error { return ErrUnsupportedEvidenceHost }

// productionSupervisorBinding is unavailable off Linux; tests inject the
// binding through the lease seam.
func productionSupervisorBinding() (supervisorBinding, error) {
	return supervisorBinding{}, ErrUnsupportedEvidenceHost
}
