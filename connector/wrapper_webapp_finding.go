package connector

import (
	"github.com/nortonlifelock/domain"
	"github.com/nortonlifelock/qualys"
	"strconv"
	"strings"
	"time"
)

type webAppFindingWrapper struct {
	f *qualys.WebAppFinding

	session *QsSession
	vuln    *vulnerabilityInfo
}

// ID returns the Aegis DB value which is not available from Qualys API
func (f *webAppFindingWrapper) ID() string {
	return ""
}

// VulnerabilityID returns the QID
func (f *webAppFindingWrapper) VulnerabilityID() string {
	return f.f.Qid
}

func (f *webAppFindingWrapper) Status() string {
	var status = f.f.StatusVal // NEW, ACTIVE, REOPENED, PROTECTED, and FIXED
	detectionType := f.f.Type  // VULNERABILITY, SENSITIVE_CONTENT, or INFORMATION_GATHERED

	const (
		info = "INFORMATION_GATHERED"
	)

	if detectionType == info {
		status = domain.Informational
	} else {
		switch strings.ToLower(status) {
		case "new":
			status = domain.Vulnerable
		case "active":
			status = domain.Vulnerable
		case "reopened":
			status = domain.Vulnerable
		case "fixed":
			status = domain.Fixed
		case "protected":
			status = domain.Vulnerable
			// TODO what does protected mean precisely?
		default:
			// do nothing
		}
	}

	return status
}

func (f *webAppFindingWrapper) ActiveKernel() *int {
	return nil
}

const (
	webAppFindingTimeFormat = "2006-01-02T15:04:05Z"
)

// Detected returns the date the finding was first found
func (f *webAppFindingWrapper) Detected() (*time.Time, error) {
	timeVal, err := time.Parse(webAppFindingTimeFormat, f.f.FirstDetectedDate)
	return &timeVal, err
}

func (f *webAppFindingWrapper) TimesSeen() int {
	timesSeen, _ := strconv.Atoi(f.f.TimesDetected)
	return timesSeen
}

func (f *webAppFindingWrapper) Proof() string {
	return ""
}

func (f *webAppFindingWrapper) Port() int {
	return 0
}

func (f *webAppFindingWrapper) Protocol() string {
	return ""
}

func (f *webAppFindingWrapper) IgnoreID() (*string, error) {
	return nil, nil
}

func (f *webAppFindingWrapper) LastFound() *time.Time {
	timeVal, _ := time.Parse(webAppFindingTimeFormat, f.f.LastDetectedDate)
	return &timeVal
}

func (f *webAppFindingWrapper) LastUpdated() *time.Time {
	return nil
}

func (f *webAppFindingWrapper) Device() (domain.Device, error) {
	return &WebAppWrapper{
		sourceID: f.f.WebApp.ID,
		name:     f.f.WebApp.Name,
		url:      f.f.WebApp.URL,
	}, nil
}

func (f *webAppFindingWrapper) Vulnerability() (domain.Vulnerability, error) {
	var err error
	if f.vuln == nil {
		qidInt, _ := strconv.Atoi(f.f.Qid)
		f.vuln = lazyLoadVulnerabilityInfo(qidInt, f.session)
	}
	return f.vuln, err
}