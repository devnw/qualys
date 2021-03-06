package qualys

import (
	"fmt"
	"github.com/nortonlifelock/log"
	"strconv"
	"strings"
	"sync"
)

func (session *Session) GetTagDetections(tags []string, kernelFilterFlag int) (out <-chan QHost, err error) {
	// Check for valid list of groups
	if tags != nil && len(tags) > 0 {
		// Handle the API request fields for Qualys

		var fields = make(map[string]string)
		fields["action"] = "list"
		fields["truncation_limit"] = "0"   // Pull groups of 2500 assets at a time until all assets are loaded
		fields["show_reopened_info"] = "1" // Show the additional information related to vulnerabilities that have been Reopened in Qualys
		fields["arf_kernel_filter"] = strconv.Itoa(kernelFilterFlag)
		fields["status"] = "New,Active,Re-Opened,Fixed" // If this parameter is not passed to the API, by default, the output contains detections with New, Active or Re-Opened <STATUS> only
		fields["use_tags"] = "1"

		// we can provide either the name of the tag, or the id of the tag
		// names are strings, ids are integers. here we check if the tags appear to be integers or strings
		if _, convertErr := strconv.Atoi(tags[0]); convertErr == nil {
			fields["tag_set_by"] = "id"
		} else {
			fields["tag_set_by"] = "name"
		}

		fields["tag_include_selector"] = "all"
		fields["tag_set_include"] = strings.Join(tags, ",")

		// TODO ec2 is currently the only metadata that Qualys supports
		fields["host_metadata"] = "ec2"
		fields["host_metadata_fields"] = "instanceId"

		session.lstream.Send(log.Infof("Loading detections for hosts tagged by [%s] from Qualys", fields["tag_set_include"]))

		out, _, err = session.getHostDetectionPostData(session.Config.Address()+qsAssetVMHost, fields)
	} else {
		err = fmt.Errorf("empty group list passed to GetHostDetections")
	}

	return out, err
}

// GetHostDetections Loads the vulnerability detections for each host that is part of the groups passed
// into the "groups" variable and returns them on the OUT channel back to the processor
// kernelFilterFlag sets the arf_kernel_filter flag in the host detection API calls. Can hold values [0,4]
// 0 vulnerabilities are not filtered based on kernel activity
// 1 exclude kernel related vulnerabilities that are not exploitable (found on non-running kernels)
// 2 only include kernel related vulnerabilities that are not exploitable (found on non-running kernels)
// 3 only include kernel related vulnerabilities that are exploitable (found on running kernels)
// 4 only include kernel related vulnerabilities
func (session *Session) GetHostDetections(groups []string, kernelFilterFlag int) (out <-chan QHost, err error) {
	// Check for valid list of groups
	if groups != nil && len(groups) > 0 {
		// Handle the API request fields for Qualys
		var fields = make(map[string]string)
		fields["action"] = "list"
		fields["truncation_limit"] = "2500"             // Pull groups of 2500 assets at a time until all assets are loaded
		fields["show_reopened_info"] = "1"              // Show the additional information related to vulnerabilities that have been Reopened in Qualys
		fields["status"] = "New,Active,Re-Opened,Fixed" // If this parameter is not passed to the API, by default, the output contains detections with New, Active or Re-Opened <STATUS> only
		fields["arf_kernel_filter"] = strconv.Itoa(kernelFilterFlag)
		fields["ag_ids"] = strings.Join(groups, ",")

		session.lstream.Send(log.Infof("Loading [%s] Hosts from Qualys", fields["truncation_limit"]))

		out, _, err = session.getHostDetectionPostData(session.Config.Address()+qsAssetVMHost, fields)
	} else {
		err = fmt.Errorf("empty group list passed to GetHostDetections")
	}

	return out, err
}

// GetHostSpecificDetections loads vulnerabilities from the Host Detection API for specific IP addresses which are passed
// into the method and returns them through the output variable as a return
// kernelFilterFlag sets the arf_kernel_filter flag in the host detection API calls. Can hold values [0,4]
// 0 vulnerabilities are not filtered based on kernel activity
// 1 exclude kernel related vulnerabilities that are not exploitable (found on non-running kernels)
// 2 only include kernel related vulnerabilities that are not exploitable (found on non-running kernels)
// 3 only include kernel related vulnerabilities that are exploitable (found on running kernels)
// 4 only include kernel related vulnerabilities
func (session *Session) GetHostSpecificDetections(ip []string, groups []string, kernelFilterFlag int) (output *QHostListDetectionOutput, err error) {

	if ip != nil && len(ip) > 0 {

		var fields = make(map[string]string)
		fields["action"] = "list"

		// TODO: Correct this so that there can be more than 10000 results and we recurse over them
		fields["truncation_limit"] = "0"   // 0 means no limit
		fields["show_reopened_info"] = "1" // Show the additional information related to vulnerabilities that have been Reopened in Qualys
		fields["status"] = "New,Active,Re-Opened,Fixed"
		fields["arf_kernel_filter"] = strconv.Itoa(kernelFilterFlag)
		fields["status"] = "New,Active,Re-Opened,Fixed" // If this parameter is not passed to the API, by default, the output contains detections with New, Active or Re-Opened <STATUS> only

		// Concatenate the IP addresses together in a comma separated list of values
		fields["ips"] = strings.Join(ip, ",")
		fields["ag_ids"] = strings.Join(groups, ",")

		output = &QHostListDetectionOutput{}

		// Execute the post call against the API
		err = session.post(session.Config.Address()+qsAssetVMHost, fields, output)
	}

	return output, err
}

// getHostDetectionPostData is a recursive API call that pulls data from the Host Detection API in steps and reads the data
// into the OUT channel which is passed back to the processor
func (session *Session) getHostDetectionPostData(path string, fields map[string]string) (outReadOnly <-chan QHost, totalHosts int, err error) {
	var out = make(chan QHost)

	go func(out chan<- QHost) {
		defer handleRoutinePanic(session.lstream)
		defer close(out)
		var output = QHostListDetectionOutput{}

		// Execute the POST call against the API
		if err = session.post(path, fields, &output); err == nil {

			// Check the length of the host slice returned from Qualys
			totalHosts = len(output.Hosts)

			session.lstream.Send(log.Infof("Host List Returned from Qualys"))

			var recursiveWG = &sync.WaitGroup{}

			// Determine if there was an error object in the return of the API call and call the next page of API
			// results from Qualys
			if output.Warning != nil {

				recursiveWG.Add(1)

				// Execute the next page load in a go routine to allow it to happen concurrently while we process the results from this call
				session.lstream.Send(log.Infof("Loading Another [%s] Hosts from Qualys", fields["truncation_limit"]))
				go func() {
					defer handleRoutinePanic(session.lstream)
					defer recursiveWG.Done()
					var extrahosts int

					var recursiveOut <-chan QHost

					// Initiate recursive call to the API to pull the next page
					if recursiveOut, extrahosts, err = session.getHostDetectionPostData(output.Warning.URL, fields); err == nil {
						totalHosts += extrahosts

						for {
							if in, ok := <-recursiveOut; ok {
								out <- in
							} else {
								break
							}
						}
					}
				}()
			}

			// Loop through the hosts returned in this call and push them to the OUT channel for processing
			session.lstream.Send(log.Infof("Processing [%v] Hosts from Qualys Host List Detection API", len(output.Hosts)))
			for _, host := range output.Hosts {
				var detects = len(host.Detections)
				// Ensure there were detections on the host before pushing it to the channel
				session.lstream.Send(log.Infof("Pushing Host [%v] with [%v] Detections to channel for processing", host.HostID, detects))
				// Push the host to the OUT channel for processing
				out <- host
			}

			recursiveWG.Wait()
		} else {
			session.lstream.Send(log.Errorf(err, "Error While Loading Host List Detections from Qualys [%s]", err.Error()))
		}
	}(out)

	return out, totalHosts, err
}

// GetHostAGInfo returns a list of host details corresponding to the IPs that were inputted
// a single IP may be provided, but is an expensive API call. It is much more efficient to query IPs in bulk
func (session *Session) GetHostAGInfo(ips []string) (output *HostListOutput, err error) {
	var fields = make(map[string]string)
	fields["action"] = "list"
	fields["ips"] = strings.Join(ips, ",")
	fields["details"] = "Basic/AGs"

	output = &HostListOutput{}
	err = session.post(session.Config.Address()+"/api/2.0/fo/asset/host/", fields, output)
	return output, err
}
