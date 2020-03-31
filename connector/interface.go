package connector

import (
	"context"
	"encoding/json"
	"github.com/nortonlifelock/domain"
	"github.com/nortonlifelock/log"
	"github.com/nortonlifelock/qualys"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KnowledgeBase grabs all vulnerabilities from the Qualys knowledge base and pushes them onto a channel
func (session *QsSession) KnowledgeBase(ctx context.Context, since *time.Time) <-chan domain.Vulnerability {
	var out = make(chan domain.Vulnerability, 50)

	go func(out chan<- domain.Vulnerability) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)
		var err error

		start := time.Now()
		if err = session.loadAndCacheQualysKB(since); err == nil {
			session.lstream.Send(log.Infof("%d vulnerabilities loaded, took %s - beginning processing", len(session.vulnerabilities), time.Since(start).Round(time.Second)))

			var wg = &sync.WaitGroup{}
			var count = 0
			for index := range session.vulnerabilities {

				select {
				case <-ctx.Done():
					return
				default:
					// Create 50 vulnerabilities at a time so we don't have tons of goroutines sitting around forever waiting to finish
					count++
					if count%50 == 0 {
						wg.Wait()
					}

					wg.Add(1)
					go func(v *qualys.QVulnerability) {
						defer handleRoutinePanic(session.lstream)
						defer wg.Done()

						select {
						case <-ctx.Done():
							return
						case out <- &vulnerabilityInfo{v: v}:
						}
					}(session.vulnerabilities[index])
				}
			}

			wg.Wait()
		} else {
			session.lstream.Send(log.Error("Error while loading vulnerabilities", err))
		}
	}(out)

	return out
}

// Detections returns a channel which contains combinations of devices/vulnerabilities within a group where the vulnerability was found on the device
// and the device exists in the asset group
func (session *QsSession) Detections(ctx context.Context, ids []string) (detections <-chan domain.Detection, err error) {
	var out = make(chan domain.Detection)

	go func(out chan<- domain.Detection) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)

		var tags = make([]string, 0)
		var groupIDs = make([]string, 0)
		const tagPrefix = "tag-"
		for _, id := range ids {
			if strings.Index(id, tagPrefix) >= 0 {
				tags = append(tags, id[strings.Index(id, tagPrefix)+len(tagPrefix):])
			} else {
				groupIDs = append(groupIDs, id)
			}
		}

		if len(groupIDs) > 0 {
			session.lstream.Send(log.Infof("Loading Detections from Qualys using group IDs [%s]", strings.Join(groupIDs, ",")))

			var hosts <-chan qualys.QHost
			if hosts, err = session.apiSession.GetHostDetections(groupIDs, session.payload.KernelFilter); err == nil {

				var processedDevVulns = make(map[string]bool)
				var devVulnMutex = &sync.Mutex{}

				wg := &sync.WaitGroup{}
				func() {
					for {
						select {
						case <-ctx.Done():
							return
						case h, ok := <-hosts:
							if ok {
								wg.Add(1)
								go func(h qualys.QHost) {
									defer handleRoutinePanic(session.lstream)
									defer wg.Done()
									session.pushCombosForHost(ctx, h, devVulnMutex, processedDevVulns, out)
								}(h)
							} else {
								return
							}
						}
					}
				}()
				wg.Wait()

			} else {
				session.lstream.Send(log.Error("Error while loading host detections from Qualys", err))
			}
		}

		if len(tags) > 0 {
			session.lstream.Send(log.Infof("Loading Detections from Qualys using tags [%s]", strings.Join(tags, ",")))

			var hosts <-chan qualys.QHost
			if hosts, err = session.apiSession.GetTagDetections(tags, session.payload.KernelFilter); err == nil {

				var processedDevVulns = make(map[string]bool)
				var devVulnMutex = &sync.Mutex{}

				wg := &sync.WaitGroup{}
				func() {
					for {
						select {
						case <-ctx.Done():
							return
						case h, ok := <-hosts:
							if ok {
								wg.Add(1)
								go func(h qualys.QHost) {
									defer handleRoutinePanic(session.lstream)
									defer wg.Done()
									session.pushCombosForHost(ctx, h, devVulnMutex, processedDevVulns, out)
								}(h)
							} else {
								return
							}
						}
					}
				}()
				wg.Wait()

			} else {
				session.lstream.Send(log.Error("Error while loading host detections from Qualys", err))
			}
		}

	}(out)

	return out, err
}

// ScanResults takes a scanID and returns a series of detections that were found by the corresponding scan
func (session *QsSession) ScanResults(ctx context.Context, payload []byte) (<-chan domain.Detection, <-chan domain.KeyValue, error) {
	var out = make(chan domain.Detection)
	var deadIPToProof = make(chan domain.KeyValue)

	go func(out chan<- domain.Detection, deadIPToProof chan<- domain.KeyValue) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)

		var needToClose = true
		defer func() {
			if needToClose {
				close(deadIPToProof)
			}
		}()

		scanInfo := &scan{session: session}
		if err := json.Unmarshal(payload, scanInfo); err == nil {

			if !scanInfo.Scheduled {
				if len(scanInfo.ScanID) > 0 {

					// Ask Qualys for the scan so we can find what IPs it scanned
					var scan qualys.ScanQualys
					scan, err = session.apiSession.GetScanByReference(scanInfo.ScanID)
					if err == nil {
						ipList := strings.Replace(scan.Target, ", ", ",", -1)

						// Use the IPs to grab the host detections
						var output *qualys.QHostListDetectionOutput
						output, err = session.apiSession.GetHostSpecificDetections(strings.Split(ipList, ","), session.payload.KernelFilter)
						if err == nil {

							var deadHostIPToProof map[string]string
							if deadHostIPToProof, err = session.getDeadHostsForScan(scanInfo.ScanID, scanInfo.Created); err == nil {

								needToClose = false
								go func() {
									defer close(deadIPToProof)

									for deadIP, proof := range deadHostIPToProof {
										select {
										case <-ctx.Done():
											return
										case deadIPToProof <- deadIPProofCombo{
											ip:    deadIP,
											proof: proof,
										}:
										}
									}
								}()

								if session.pushDetectionsOnChannel(ctx, output, deadHostIPToProof, out) {
									return
								}
							} else {
								session.lstream.Send(log.Errorf(err, "error while loading dead hosts for scan %v", scanInfo.ScanID))
							}

							// TODO refactor this to own method
							if scanInfo.TemplateID != strconv.Itoa(session.payload.DiscoveryOptionProfileID) && scanInfo.TemplateID != strconv.Itoa(session.payload.OptionProfileID) {
								if len(scanInfo.TemplateID) > 0 {
									templateFields := strings.Split(scanInfo.TemplateID, templateDelimiter)
									if err = session.apiSession.DeleteOptionProfile(templateFields[0]); err != nil {
										session.lstream.Send(log.Errorf(err, "error while deleting option profile for scan %v", scanInfo.ScanID))
									}

									// search list only exists in vulnerability scans
									if len(templateFields) > 1 {
										if err = session.apiSession.DeleteSearchList(templateFields[1]); err != nil {
											session.lstream.Send(log.Errorf(err, "error while deleting search list for scan %v", scanInfo.ScanID))
										}
									}
								} else {
									session.lstream.Send(log.Warningf(err, "no template found in payload of scan %v", scanInfo.ScanID))
								}
							} else {
								// do nothing - we don't want to delete the option profile which we make copies of
								// this block should never hit, but we keep it just in case
							}
						} else {
							session.lstream.Send(log.Errorf(err, "error while getting host detections for scan %v", scanInfo.ScanID))
						}
					} else {
						session.lstream.Send(log.Errorf(err, "error while gathering the scan %v", scanInfo.ScanID))
					}
				} else {
					session.lstream.Send(log.Errorf(err, "zero length scan ID received in payload"))
				}
			} else {
				if len(scanInfo.GroupID) > 0 {
					detections, err := session.Detections(ctx, strings.Split(scanInfo.GroupID, ","))
					if err == nil {
						for {
							select {
							case <-ctx.Done():
								return
							case val, ok := <-detections:
								if ok {
									select {
									case <-ctx.Done():
										return
									case out <- val:
									}
								} else {
									break
								}
							}
						}
					} else {
						session.lstream.Send(log.Errorf(err, "error while loading detections for [%s]", scanInfo.GroupID))
					}
				} else {
					session.lstream.Send(log.Errorf(err, "Scheduled scan [%s] did not specify the group IDs or cloud tags that it executed against"))
				}
			}

		} else {
			session.lstream.Send(log.Errorf(err, "error while unmarshalling scan"))
		}

	}(out, deadIPToProof)

	return out, deadIPToProof, nil
}

func (session *QsSession) pushDetectionsOnChannel(ctx context.Context, output *qualys.QHostListDetectionOutput, deadHostIPToProof map[string]string, out chan<- domain.Detection) bool {
	for _, h := range output.Hosts {
		for _, d := range h.Detections {

			var deadHostProof = deadHostIPToProof[h.IPAddress]
			if len(deadHostProof) > 0 {
				d.Status = domain.DeadHost
				d.Proof = deadHostProof
			} else if d.Status == "Fixed" {
				d.Status = domain.Fixed
			}

			select {
			case <-ctx.Done():
				return true
			case out <- &hostDetectionCombo{
				host: &host{
					h: h,
				},
				detection: &detection{
					d:       d,
					session: session,
				},
			}:
			}
		}
	}

	return false
}

type deadIPProofCombo struct {
	ip    string
	proof string
}

func (d deadIPProofCombo) Key() string {
	return d.ip
}

func (d deadIPProofCombo) Value() string {
	return d.proof
}

// Discovery kicks of a Qualys scan to identify which devices corresponding to the IPs are online
func (session *QsSession) Discovery(ctx context.Context, matches []domain.Match) (scanID <-chan domain.Scan) {
	var out = make(chan domain.Scan)

	go func(out chan<- domain.Scan) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)

		// make a list of unique IPs (in case we were provided a slice with duplicates) so we can assign them to a group
		var seen = make(map[string]bool)
		var uniqueIPs = make([]string, 0)
		for _, match := range matches {
			if !seen[match.IP()] {
				seen[match.IP()] = true
				uniqueIPs = append(uniqueIPs, match.IP())
			}
		}

		var err error
		var groupIDToScanBundle map[int]*scanBundle
		if groupIDToScanBundle, err = session.prepareIPsAndAGMapping(uniqueIPs); err == nil {
			// wg to ensure we don't close the out channel before the threads finish
			wg := &sync.WaitGroup{}

			for groupID := range groupIDToScanBundle {
				wg.Add(1)
				go func(bundle *scanBundle) {
					defer handleRoutinePanic(session.lstream)
					defer wg.Done()

					// error intentionally scoped out
					err := session.createDiscoveryScanForGroup(ctx, out, bundle)
					if err != nil {
						session.lstream.Send(log.Errorf(err, "error while creating scan for group %v", bundle.groupID))
					}
				}(groupIDToScanBundle[groupID])
			}

			wg.Wait()
		} else {
			session.lstream.Send(log.Errorf(err, "error while creating assignment group mapping for discovery scan"))
		}
	}(out)

	return out
}

// Scan creates a Qualys scan for all the devices/vulnerabilities passed onto the channel. All vulnerabilities passed will be searched for on all devices
// passed. The scanID is passed onto a channel before closing the channel immediately.
func (session *QsSession) Scan(ctx context.Context, detections []domain.Match) (scanID <-chan domain.Scan, err error) {
	var out = make(chan domain.Scan)

	go func(out chan<- domain.Scan) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)

		session.createScanForDetections(ctx, detections, out)
	}(out)

	return out, err
}

// Scans takes a channel of scan payloads, and unmarshals into a scan struct that implements the Scan interface
// these interfaces are then used to gather the statuses of the scans by the caller
func (session *QsSession) Scans(ctx context.Context, payloads <-chan []byte) (scans <-chan domain.Scan) {
	var out = make(chan domain.Scan)

	go func(out chan<- domain.Scan) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)

		var seen = make(map[string]bool)
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-payloads:
				if ok {
					scan := &scan{session: session}
					if err := json.Unmarshal(payload, scan); err == nil {

						if len(scan.ScanID) > 0 {
							seen[scan.Name] = true
							select {
							case <-ctx.Done():
								return
							case out <- scan:
							}
						} else if !seen[scan.Name] && len(scan.Name) > 0 {
							// this block hits when the title for an expected scheduled scan was passed instead of a scan reference
							// here we must check to see if one of those scan schedules actually have a scan running, if it does - we push it on the channel

							// the empty scan ID means that a scheduled scan isn't running with that name currently, or the recently created scan with that name hasn't had it's scan ID
							// loaded yet. We check for a running scan and populate the scan ID of the scheduled
							scheduledScan, err := session.apiSession.GetScheduledScan(scan.Name)
							if err == nil {
								if scheduledScan != nil {

									tagsCoveredByScheduledScan, err := session.apiSession.GetAssetTagTargetOfScheduledScan(scan.Name)
									if err == nil {
										seen[scan.Name] = true
										scan.ScanID = scheduledScan.Reference
										scan.Created = scheduledScan.LaunchDate
										scan.GroupID = tagsCoveredByScheduledScan
										scan.Scheduled = true

										select {
										case <-ctx.Done():
											return
										case out <- scan:
										}
									} else {
										session.lstream.Send(log.Errorf(err, "error while loading the asset tag target of [%s]", scan.Name))
									}
								}
							} else {
								session.lstream.Send(log.Errorf(err, "error while finding scheduled scan for [%s]", scan.Name))
							}
						}
					} else {
						session.lstream.Send(log.Errorf(err, "error while marshaling scan"))
					}
				} else {
					return
				}
			}
		}
	}(out)

	return out
}
