package messageprocessor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/carbonblack/cb-event-forwarder/internal/cbapi"
	"github.com/carbonblack/cb-event-forwarder/internal/deepcopy"
	"github.com/carbonblack/cb-event-forwarder/internal/util"
	log "github.com/sirupsen/logrus"
)

type JSONMessageHandlerFunc func(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error)

type JsonMessageProcessor struct {
	DebugFlag       bool
	DebugStore      string
	CbServerURL     string
	EventMap        map[string]bool
	CbAPI           *cbapi.CbAPIHandler
	messageHandlers map[string]JSONMessageHandlerFunc
}

var feedParserRegex = regexp.MustCompile(`^feed\.(\d+)\.(.*)$`)

func parseQueryString(encodedQuery map[string]string) (queryIndex string, parsedQuery string, err error) {
	err = nil

	queryIndex, ok := encodedQuery["index_type"]
	if !ok {
		err = errors.New("no index_type included in query")
		return
	}

	rawQuery, ok := encodedQuery["search_query"]
	if !ok {
		err = errors.New("no search_query included in query")
		return
	}

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return
	}

	queryArray, ok := query["q"]
	if !ok {
		err = errors.New("no 'q' query parameter provided")
		return
	}

	parsedQuery = queryArray[0]
	return
}

func handleKeyValues(msg map[string]interface{}) {
	var alliance_data_map = make(map[string]map[string]interface{}, 0)
	for key, value := range msg {
		switch {
		case strings.Contains(key, "alliance_"):
			alliance_data := strings.Split(key, "_")
			if len(alliance_data) == 3 {
				alliance_data_source := alliance_data[2]
				alliance_data_key := alliance_data[1]
				alliance_map, alreadyexists := alliance_data_map[alliance_data_source]
				if alreadyexists {
					alliance_map[alliance_data_key] = value
				} else {
					temp := make(map[string]interface{})
					temp[alliance_data_key] = value
					alliance_data_map[alliance_data_source] = temp
				}
			}
			delete(msg, key)
		case key == "endpoint":
			endpointstr := ""
			switch value.(type) {
			case string:
				endpointstr = value.(string)
			case []interface{}:
				endpointstr = value.([]interface{})[0].(string)
			}
			parts := strings.Split(endpointstr, "|")
			hostname := parts[0]
			sensorID := parts[1]
			msg["hostname"] = hostname
			msg["sensor_id"] = sensorID
			delete(msg, "endpoint")
		case key == "highlights_by_doc":
			delete(msg, "highlights_by_doc")
		case key == "highlights":
			delete(msg, "highlights")
		/*case key == "event_timestamp":
		msg["timestamp"] = value
		delete(msg, "event_timestamp")*/
		case key == "timestamp":
			msg["event_timestamp"] = value
			delete(msg, "timestamp")
		case key == "computer_name":
			msg["hostname"] = value
			delete(msg, "computer_name")
		case key == "md5" || key == "parent_md5" || key == "process_md5":
			if md5, ok := value.(string); ok {
				if len(md5) == 32 {
					msg[key] = strings.ToUpper(md5)
				}
			}
		case key == "ioc_type":
			// if the ioc_type is a map and it contains a key of "md5", uppercase it
			v := reflect.ValueOf(value)
			if v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String {
				iocType := value.(map[string]interface{})
				if md5value, ok := iocType["md5"]; ok {
					if md5, ok := md5value.(string); ok {
						if len(md5) != 32 && len(md5) != 0 {
							log.WithFields(log.Fields{"MD5 Length": len(md5)}).Warn("MD5 Length was not valid")
						}
						iocType["md5"] = strings.ToUpper(md5)
					}
				}
			} else {
				if iocType, ok := value.(string); ok {
					if iocType == "query" {
						// decode the IOC query
						if rawIocValue, ok := msg["ioc_value"].(string); ok {
							var iocValue map[string]string
							if json.Unmarshal([]byte(rawIocValue), &iocValue) == nil {
								if queryIndex, rawQuery, err := parseQueryString(iocValue); err == nil {
									msg["ioc_query_index"] = queryIndex
									msg["ioc_query_string"] = rawQuery
								}
							}
						}
					}
				}
			}
		case key == "comms_ip" || key == "interface_ip":
			if value, ok := value.(json.Number); ok {
				ipaddr, err := strconv.ParseInt(value.String(), 10, 32)
				if err == nil {
					msg[key] = util.GetIPv4AddressSigned(int32(ipaddr))
				}
			}
		}
	}
	if len(alliance_data_map) > 0 {
		msg["alliance_data"] = alliance_data_map
	}
}

func (jsp *JsonMessageProcessor) addLinksToMessage(msg map[string]interface{}) {
	if jsp.CbServerURL == "" {
		return
	}

	// add sensor links when applicable
	if value, ok := msg["sensor_id"]; ok {
		if value, ok := value.(json.Number); ok {
			hostID, err := strconv.ParseInt(value.String(), 10, 32)
			if err == nil {
				msg["link_sensor"] = fmt.Sprintf("%s#/host/%d", jsp.CbServerURL, hostID)
			}
		}
	}

	// add binary links when applicable
	for _, key := range [...]string{"md5", "parent_md5", "process_md5"} {
		if value, ok := msg[key]; ok {
			if md5, ok := value.(string); ok {
				if len(md5) == 32 {
					keyName := "link_" + key
					msg[keyName] = fmt.Sprintf("%s#/binary/%s", jsp.CbServerURL, msg[key])
				}
			}
		}
	}

	// add process links
	if processGUID, ok := msg["process_guid"]; ok {
		if processID, segmentID, err := util.ParseFullGUID(processGUID.(string)); err == nil {
			msg["link_process"] = fmt.Sprintf("%s#analyze/%v/%v", jsp.CbServerURL, processID, segmentID)
		}
	}

	if parentGUID, ok := msg["parent_guid"]; ok {
		if parentID, segmentID, err := util.ParseFullGUID(parentGUID.(string)); err == nil {
			msg["link_parent"] = fmt.Sprintf("%s#analyze/%v/%v", jsp.CbServerURL, parentID, segmentID)
		}
	}
}

func fixupMessageType(routingKey string) string {
	if feedParserRegex.MatchString(routingKey) {
		return fmt.Sprintf("feed.%s", feedParserRegex.FindStringSubmatch(routingKey)[2])
	}
	return routingKey
}

func (jsp *JsonMessageProcessor) ProcessJSONMessage(msg map[string]interface{}, routingKey string) ([]map[string]interface{}, error) {
	messageType := fixupMessageType(routingKey)

	if processfunc, ok := jsp.messageHandlers[messageType]; ok {
		outmsgs, err := processfunc(messageType, msg)

		// add links for each message
		for _, outmsg := range outmsgs {
			jsp.addLinksToMessage(outmsg)
		}
		return outmsgs, err
	}

	return nil, nil
}

/*
 * PostprocessJSONMessage performs postprocessing on feed/watchlist/alert messages.
 * For exmaple, for feed hits we need to grab the report_title.
 * To do this we must query the Cb Response Server's REST API to get the report_title.  NOTE: In order to do this
 * functionality we need the Cb Response Server URL and API Token set within the config.
 */
func (jsp *JsonMessageProcessor) PostprocessJSONMessage(msg map[string]interface{}) map[string]interface{} {

	feedID, feedIDPresent := msg["feed_id"]
	reportID, reportIDPresent := msg["report_id"]

	/*
		:/p			 * First make sure these fields are present
	*/
	if feedIDPresent && reportIDPresent {
		/*
		 * feedID should be of type json.Number which is typed as a string
		 * reportID should be of type string as well
		 */
		if reflect.TypeOf(feedID).Kind() == reflect.String &&
			reflect.TypeOf(reportID).Kind() == reflect.String {
			iFeedID, err := feedID.(json.Number).Int64()
			if err == nil {
				/*
				 * Get the report_title for this feed hit
				 */
				reportTitle, reportScore, reportLink, err := jsp.CbAPI.GetReport(int(iFeedID), reportID.(string))
				log.Debugf("Report title = %s , Score = %d, link = %s", reportTitle, reportScore, reportLink)
				if err == nil {
					/*
					 * Finally save the report_title into this message
					 */
					msg["report_title"] = reportTitle
					msg["report_score"] = reportScore
					msg["report_link"] = reportLink
					/*
						log.Infof("report title for id %s:%s == %s\n",
							feedID.(json.Number).String(),
							reportID.(string),
							reportTitle)
					*/
				}

			} else {
				log.Info("Unable to convert feed_id to int64 from json.Number")
			}

		} else {
			log.Info("Feed Id was an unexpected type")
		}
	}
	return msg
}

func getString(m map[string]interface{}, k string, dv string) string {
	if val, ok := m[k]; ok {
		if strval, ok := val.(string); ok {
			return strval
		}
	}
	return dv
}

func getNumber(m map[string]interface{}, k string, dv json.Number) json.Number {
	if val, ok := m[k]; ok {
		if numval, ok := val.(json.Number); ok {
			return numval
		}
	}
	return dv
}

func getIPAddress(m map[string]interface{}, k string, dv string) string {
	if val, ok := m[k]; ok {
		if numval, ok := val.(json.Number); ok {
			ipaddr, err := strconv.ParseInt(numval.String(), 10, 32)
			if err == nil {
				return util.GetIPv4AddressSigned(int32(ipaddr))
			}
		} else if strval, ok := val.(string); ok {
			return strval
		}
	}
	return dv
}

// returns nil if the boolean can't be decoded
func getBool(m map[string]interface{}, k string) interface{} {
	if val, ok := m[k]; ok {
		if boolval, ok := val.(bool); ok {
			return boolval
		}
	}
	return nil
}

func copySensorMetadata(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	// sensor metadata
	outmsg["sensor_id"] = getNumber(subdoc, "sensor_id", json.Number("0"))
	outmsg["hostname"] = getString(subdoc, "hostname", "")
	outmsg["group"] = getString(subdoc, "group", "")
	outmsg["comms_ip"] = getIPAddress(subdoc, "comms_ip", "")
	outmsg["interface_ip"] = getIPAddress(subdoc, "interface_ip", "")
	outmsg["host_type"] = getString(subdoc, "host_type", "")
	outmsg["os_type"] = getString(subdoc, "os_type", "")
}

func copyFeedSensorMetadata(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	// feed messages do not include comms_ip, interface_ip, or host_type
	outmsg["sensor_id"] = getNumber(subdoc, "sensor_id", json.Number("0"))
	outmsg["hostname"] = getString(subdoc, "hostname", "")
	outmsg["group"] = getString(subdoc, "group", "")
	outmsg["os_type"] = getString(subdoc, "os_type", "")
}

func copyProcessMetadata(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	// process metadata
	outmsg["process_md5"] = strings.ToUpper(getString(subdoc, "process_md5", ""))
	outmsg["process_guid"] = getString(subdoc, "unique_id", "")
	outmsg["process_name"] = getString(subdoc, "process_name", "")
	outmsg["cmdline"] = getString(subdoc, "cmdline", "")
	outmsg["process_pid"] = getNumber(subdoc, "process_pid", json.Number("0"))
	outmsg["username"] = getString(subdoc, "username", "")
	outmsg["path"] = getString(subdoc, "path", "")
	outmsg["last_update"] = getString(subdoc, "last_update", "")
	outmsg["start"] = getString(subdoc, "start", "")
}

func copyParentMetadata(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	// parent process metadata
	outmsg["parent_name"] = getString(subdoc, "parent_name", "")
	outmsg["parent_guid"] = getString(subdoc, "parent_unique_id", "")
	outmsg["parent_pid"] = getNumber(subdoc, "parent_pid", json.Number("0"))
}

func copyEventCounts(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	// process event counts at the time the watchlist/feed/alert hit occurred
	for _, count := range []string{
		"modload_count", "filemod_count", "regmod_count", "emet_count",
		"netconn_count", "crossproc_count", "processblock_count",
		"childproc_count",
	} {
		outmsg[count] = getNumber(subdoc, count, json.Number("0"))
	}
}

func copyBinaryMetadata(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	// binary metadata
	outmsg["digsig_result"] = getString(subdoc, "digsig_result", "(unknown)")
	outmsg["digsig_result_code"] = getString(subdoc, "digsig_result_code", "")
	outmsg["product_version"] = getString(subdoc, "product_version", "")
	outmsg["copied_mod_len"] = getNumber(subdoc, "copied_mod_len", json.Number("0"))
	outmsg["orig_mod_len"] = getNumber(subdoc, "orig_mod_len", json.Number("0"))
	outmsg["is_executable_image"] = getBool(subdoc, "is_executable_image")
	outmsg["is_64bit"] = getBool(subdoc, "is_64bit")
	outmsg["md5"] = getString(subdoc, "md5", "")
	outmsg["file_version"] = getString(subdoc, "file_version", "")
	outmsg["internal_name"] = getString(subdoc, "internal_name", "")
	outmsg["company_name"] = getString(subdoc, "company_name", "")
	outmsg["original_filename"] = getString(subdoc, "original_filename", "")
	outmsg["os_type"] = getString(subdoc, "os_type", "")
	outmsg["file_desc"] = getString(subdoc, "file_desc", "")
	outmsg["product_name"] = getString(subdoc, "product_name", "")
	outmsg["legal_copyright"] = getString(subdoc, "legal_copyright", "")
}

var exists = struct{}{}

// convert a list of key/value pairs such as:
// "alliance_data_bit9advancedthreats": "066eb0b2-f25b-48dc-85ad-ad20b783a25e",
// "alliance_score_bit9advancedthreats": 100,
// "alliance_link_bit9advancedthreats": "https://www.carbonblack.com/cbfeeds/advancedthreat_feed.xhtml#6",
// "alliance_updated_bit9advancedthreats": "2016-12-06T14:30:48.000Z",
//
// into:
// "alliance_data": [
//   {
//     "source": "bit9advancedthreats",
//     "data": "066eb0b2-f25b-48dc-85ad-ad20b783a25e",
//     "score": 100,
//     "link": "https://www.carbonblack.com/cbfeeds/advancedthreat_feed.xhtml#6",
//     "updated": "2016-12-06T14:30:48.000Z"
//   }
// ]
func copyAllianceInformation(subdoc map[string]interface{}, outmsg map[string]interface{}) {
	var alliance_data_list = make([]map[string]interface{}, 0)
	var alliance_data_sources = make(map[string]struct{})

	// first get a list of alliance sources
	for key := range subdoc {
		if strings.HasPrefix(key, "alliance_") {
			alliance_data := strings.Split(key, "_")
			alliance_data_source := alliance_data[2]
			alliance_data_sources[alliance_data_source] = exists
		}
	}

	// now, for each alliance source, get the ["data", "score", "link", and "updated"] fields
	for source := range alliance_data_sources {
		temp := make(map[string]interface{})
		temp["source"] = source
		// TODO: for now we take the last 'data' item from the list. The other 'alliance' fields are singletons,
		// so it doesn't make any sense to make this an array...
		if val, ok := subdoc["alliance_data_"+source]; ok {
			if sliceval, ok := val.([]string); ok {
				// take the last item from the list
				numelements := len(sliceval)
				temp["data"] = sliceval[numelements-1]
			} else if strval, ok := val.(string); ok {
				temp["data"] = strval
			} else {
				temp["data"] = ""
			}
		}
		temp["score"] = getNumber(subdoc, "alliance_score_"+source, json.Number("0"))
		temp["link"] = getString(subdoc, "alliance_link_"+source, "")
		temp["updated"] = getString(subdoc, "alliance_updated_"+source, "")
		alliance_data_list = append(alliance_data_list, temp)
	}

	outmsg["alliance_data"] = alliance_data_list
}

func (jsp *JsonMessageProcessor) watchlistHitProcess(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	// collect fields that are used across all the docs
	watchlistName := getString(inmsg, "watchlist_name", "")
	watchlistID := getNumber(inmsg, "watchlist_id", json.Number("0"))
	cbVersion := getString(inmsg, "cb_version", "")
	eventTimestamp := getNumber(inmsg, "event_timestamp", json.Number("0"))

	outmsgs := make([]map[string]interface{}, 0, 1)

	// explode watchlist/feed hit messages that include a "docs" array
	if val, ok := inmsg["docs"]; ok {
		if subdocs, ok := val.([]interface{}); ok {
			for _, submsg := range subdocs {
				if subdoc, ok := submsg.(map[string]interface{}); ok {
					outmsg := make(map[string]interface{})

					// message metadata
					outmsg["type"] = "watchlist.hit.process"
					outmsg["schema_version"] = 2

					// watchlist metadata
					outmsg["watchlist_name"] = watchlistName
					outmsg["watchlist_id"] = watchlistID

					// event metadata
					outmsg["cb_version"] = cbVersion
					outmsg["event_timestamp"] = eventTimestamp

					copySensorMetadata(subdoc, outmsg)
					copyProcessMetadata(subdoc, outmsg)
					copyParentMetadata(subdoc, outmsg)
					copyEventCounts(subdoc, outmsg)

					// append the message to our output
					outmsgs = append(outmsgs, outmsg)
				}
			}
		}
	}

	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) watchlistStorageHitProcess(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	// collect fields that are used across all the docs
	watchlistName := getString(inmsg, "watchlist_name", "")
	watchlistID := getNumber(inmsg, "watchlist_id", json.Number("0"))
	cbVersion := getString(inmsg, "cb_version", "")
	eventTimestamp := getNumber(inmsg, "event_timestamp", json.Number("0"))

	outmsgs := make([]map[string]interface{}, 0, 1)

	// explode watchlist/feed hit messages that include a "docs" array
	if val, ok := inmsg["docs"]; ok {
		if subdocs, ok := val.([]interface{}); ok {
			for _, submsg := range subdocs {
				if subdoc, ok := submsg.(map[string]interface{}); ok {
					outmsg := make(map[string]interface{})

					// message metadata
					outmsg["type"] = "watchlist.storage.hit.process"
					outmsg["schema_version"] = 2

					// watchlist metadata
					outmsg["watchlist_name"] = watchlistName
					outmsg["watchlist_id"] = watchlistID

					// event metadata
					outmsg["cb_version"] = cbVersion
					outmsg["event_timestamp"] = eventTimestamp

					// sensor metadata not available in .storage.hit events

					copyProcessMetadata(subdoc, outmsg)
					copyParentMetadata(subdoc, outmsg)
					copyEventCounts(subdoc, outmsg)

					// append the message to our output
					outmsgs = append(outmsgs, outmsg)
				}
			}
		}
	}

	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) feedIngressHitProcess(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsg := make(map[string]interface{})

	// message metadata
	outmsg["type"] = "feed.ingress.hit.process"
	outmsg["schema_version"] = 2

	// feed metadata
	outmsg["feed_name"] = getString(inmsg, "feed_name", "")
	outmsg["feed_id"] = getNumber(inmsg, "feed_id", json.Number("0"))

	// event metadata
	outmsg["cb_version"] = getString(inmsg, "cb_version", "")
	outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))

	// report metadata
	outmsg["report_id"] = getString(inmsg, "report_id", "")
	outmsg["report_score"] = getNumber(inmsg, "report_score", json.Number("0"))

	// sensor metadata
	copyFeedSensorMetadata(inmsg, outmsg)

	// report IOC attributes
	outmsg["ioc_type"] = getString(inmsg, "ioc_type", "")
	outmsg["ioc_value"] = getString(inmsg, "ioc_value", "")

	// TODO: for IP address feed hits, ioc_attr includes the full src/dest IP address info for the
	// offending netconn. The src/dest IP addresses are signed integers, however. They need to be
	// converted into dotted quad strings.
	//
	// for other events, ioc_attr includes a 'highlights' field which serves just to confuse more than
	// anything. For now let's remove it.

	// if val, ok := inmsg["ioc_attr"]; ok {
	// 	if objval, ok := val.(map[string]interface{}); ok {
	// 		outmsg["ioc_attr"] = deepcopy.Iface(objval).(map[string]interface{})
	// 	} else {
	// 		outmsg["ioc_attr"] = make(map[string]interface{})
	// 	}
	// } else {
	// 	outmsg["ioc_attr"] = make(map[string]interface{})
	// }

	// NOTE in this case the GUID is missing the segment ID. We don't have that yet.
	outmsg["process_guid"] = getString(inmsg, "process_id", "")

	outmsgs := make([]map[string]interface{}, 0, 1)
	outmsgs = append(outmsgs, outmsg)
	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) feedStorageHitProcess(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsgs := make([]map[string]interface{}, 0, 1)

	// explode watchlist/feed hit messages that include a "docs" array
	if val, ok := inmsg["docs"]; ok {
		if subdocs, ok := val.([]interface{}); ok {
			for _, submsg := range subdocs {
				if subdoc, ok := submsg.(map[string]interface{}); ok {
					outmsg := make(map[string]interface{})
					// message metadata
					outmsg["type"] = msgtype
					outmsg["schema_version"] = 2

					// feed metadata
					outmsg["feed_name"] = getString(inmsg, "feed_name", "")
					outmsg["feed_id"] = getNumber(inmsg, "feed_id", json.Number("0"))

					// event metadata
					outmsg["cb_version"] = getString(inmsg, "cb_version", "")
					outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))

					// report metadata
					outmsg["report_id"] = getString(inmsg, "report_id", "")
					outmsg["report_score"] = getNumber(inmsg, "report_score", json.Number("0"))

					if msgtype == "feed.storage.hit.process" {
						// get alliance data
						copyAllianceInformation(inmsg, outmsg)
					}

					// sensor metadata
					// note that we have to be clever here since some keys are in the 'docs' array, others are not
					outmsg["sensor_id"] = getNumber(inmsg, "sensor_id", json.Number("0"))
					outmsg["hostname"] = getString(inmsg, "hostname", "")
					outmsg["group"] = getString(inmsg, "group", "")
					outmsg["comms_ip"] = getIPAddress(inmsg, "comms_ip", "")
					outmsg["interface_ip"] = getIPAddress(inmsg, "interface_ip", "")
					outmsg["host_type"] = getString(subdoc, "host_type", "")
					outmsg["os_type"] = getString(subdoc, "os_type", "")

					// process metadata
					copyProcessMetadata(subdoc, outmsg)
					copyParentMetadata(subdoc, outmsg)
					copyEventCounts(subdoc, outmsg)

					outmsgs = append(outmsgs, outmsg)
				}
			}
		}
	}

	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) watchlistHitBinary(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsgs := make([]map[string]interface{}, 0, 1)

	// collect fields that are used across all the docs
	watchlistName := getString(inmsg, "watchlist_name", "")
	watchlistID := getNumber(inmsg, "watchlist_id", json.Number("0"))
	cbVersion := getString(inmsg, "cb_version", "")
	eventTimestamp := getNumber(inmsg, "event_timestamp", json.Number("0"))

	if val, ok := inmsg["docs"]; ok {
		if subdocs, ok := val.([]interface{}); ok {
			for _, submsg := range subdocs {
				if subdoc, ok := submsg.(map[string]interface{}); ok {
					outmsg := make(map[string]interface{})
					// message metadata
					outmsg["type"] = "watchlist.hit.binary"
					outmsg["schema_version"] = 2

					// watchlist metadata
					outmsg["watchlist_name"] = watchlistName
					outmsg["watchlist_id"] = watchlistID

					// event metadata
					outmsg["cb_version"] = cbVersion
					outmsg["event_timestamp"] = eventTimestamp
					outmsg["host_count"] = getNumber(subdoc, "host_count", json.Number("0"))
					outmsg["last_seen"] = getString(subdoc, "last_seen", "")

					copyBinaryMetadata(subdoc, outmsg)

					// details on the endpoints this was found on... these are arrays
					// TODO: endpoint should be split apart into its separate fields
					for _, endpointdetail := range []string{"observed_filename", "endpoint", "group"} {
						if val, ok := inmsg[endpointdetail]; ok {
							if objval, ok := val.(map[string]interface{}); ok {
								outmsg[endpointdetail] = deepcopy.Iface(objval).(map[string]interface{})
							} else {
								outmsg[endpointdetail] = make(map[string]interface{})
							}
						} else {
							outmsg[endpointdetail] = make(map[string]interface{})
						}
					}

					outmsgs = append(outmsgs, outmsg)
				}
			}
		}
	}
	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) watchlistStorageHitBinary(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsgs := make([]map[string]interface{}, 0, 1)

	// collect fields that are used across all the docs
	watchlistName := getString(inmsg, "watchlist_name", "")
	watchlistID := getNumber(inmsg, "watchlist_id", json.Number("0"))
	cbVersion := getString(inmsg, "cb_version", "")
	eventTimestamp := getNumber(inmsg, "event_timestamp", json.Number("0"))

	if val, ok := inmsg["docs"]; ok {
		if subdocs, ok := val.([]interface{}); ok {
			for _, submsg := range subdocs {
				if subdoc, ok := submsg.(map[string]interface{}); ok {
					outmsg := make(map[string]interface{})
					// message metadata
					outmsg["type"] = "watchlist.storage.hit.binary"
					outmsg["schema_version"] = 2

					// watchlist metadata
					outmsg["watchlist_name"] = watchlistName
					outmsg["watchlist_id"] = watchlistID

					// event metadata
					outmsg["cb_version"] = cbVersion
					outmsg["event_timestamp"] = eventTimestamp
					outmsg["host_count"] = getNumber(subdoc, "host_count", json.Number("0"))
					outmsg["last_seen"] = getString(subdoc, "last_seen", "")

					copyBinaryMetadata(subdoc, outmsg)

					outmsg["observed_filename"] = getString(subdoc, "observed_filename", "")
					endpoint := getString(subdoc, "endpoint", "")

					// split endpoint into hostname and sensor_id
					sensorparts := strings.Split(endpoint, "|")
					if len(sensorparts) == 2 {
						hostname := sensorparts[0]
						sensorID := sensorparts[1]
						outmsg["hostname"] = hostname
						outmsg["sensor_id"], _ = strconv.ParseInt(sensorID, 10, 64)
					}

					outmsg["group"] = getString(subdoc, "group", "")
					outmsg["observed_filename"] = getString(subdoc, "observed_filename", "")

					outmsgs = append(outmsgs, outmsg)
				}
			}
		}
	}

	return outmsgs, nil
}

func copyAlertMetadata(msgtype string, inmsg map[string]interface{}, outmsg map[string]interface{}) error {
	// TODO: determine whether this is a feed or watchlist hit
	feedId, err := getNumber(inmsg, "feed_id", json.Number("-1")).Int64()
	if feedId == -1 {
		// this is a watchlist hit, treat appropriately
		// watchlist metadata
		outmsg["watchlist_name"] = getString(inmsg, "watchlist_name", "")

		// watchlist IDs are strings in the input, change to integer
		outmsg["watchlist_id"] = json.Number(getString(inmsg, "watchlist_id", "0"))
	} else if err == nil {
		// this is a feed hit, treat appropriately
		// feed metadata
		outmsg["feed_name"] = getString(inmsg, "feed_name", "")
		outmsg["feed_id"] = getNumber(inmsg, "feed_id", json.Number("0"))
		outmsg["report_id"] = getString(inmsg, "watchlist_id", "")
		outmsg["feed_rating"] = getNumber(inmsg, "feed_rating", json.Number("0"))
	} else {
		return fmt.Errorf("Could not parse feed_id from incoming alert message: %s", err.Error())
	}

	// TODO: what's the score for a watchlist hit?
	outmsg["report_score"] = getNumber(inmsg, "report_score", json.Number("0"))

	// add alert metadata
	outmsg["id"] = getString(inmsg, "unique_id", "")
	outmsg["alert_severity"] = getNumber(inmsg, "alert_severity", json.Number("0"))
	outmsg["ioc_confidence"] = getNumber(inmsg, "ioc_confidence", json.Number("0"))
	outmsg["sensor_criticality"] = getNumber(inmsg, "sensor_criticality", json.Number("0"))
	outmsg["alert_type"] = getString(inmsg, "alert_type", "")
	outmsg["status"] = getString(inmsg, "status", "")

	// report IOC attributes
	outmsg["ioc_type"] = getString(inmsg, "ioc_type", "")

	// TODO: for this message type, ioc_attr is a string-ified JSON (other event types are the actual object)
	//  thoughts?

	// `ioc_value` not available for alert.watchlist.hit.query.process events
	if msgtype != "alert.watchlist.hit.query.process" {
		outmsg["ioc_value"] = getString(inmsg, "ioc_value", "")
	}

	return nil
}

func (jsp *JsonMessageProcessor) alertWatchlistHitProcess(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsg := make(map[string]interface{})

	// message metadata
	outmsg["type"] = msgtype
	outmsg["schema_version"] = 2

	err := copyAlertMetadata(msgtype, inmsg, outmsg)
	if err != nil {
		return nil, err
	}

	// copy event counts
	copyEventCounts(inmsg, outmsg)

	// sensor metadata includes everything but host_type
	outmsg["sensor_id"] = getNumber(inmsg, "sensor_id", json.Number("0"))
	outmsg["hostname"] = getString(inmsg, "hostname", "")
	outmsg["group"] = getString(inmsg, "group", "")
	outmsg["comms_ip"] = getIPAddress(inmsg, "comms_ip", "")
	outmsg["interface_ip"] = getIPAddress(inmsg, "interface_ip", "")
	outmsg["os_type"] = getString(inmsg, "os_type", "")

	// process metadata
	outmsg["process_name"] = getString(inmsg, "process_name", "")
	outmsg["process_path"] = getString(inmsg, "process_path", "")
	outmsg["username"] = getString(inmsg, "username", "")
	// TODO: is 'md5' guaranteed to be the process md5?
	outmsg["process_md5"] = getString(inmsg, "md5", "")

	// TODO: created_time appears to be event_timestamp just in string form. Eliminate one?
	outmsg["created_time"] = getString(inmsg, "created_time", "")
	outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))
	outmsg["process_guid"] = getString(inmsg, "process_unique_id", "")

	outmsgs := make([]map[string]interface{}, 0, 1)
	outmsgs = append(outmsgs, outmsg)
	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) binarystoreFileAdded(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsg := make(map[string]interface{})

	// message metadata
	outmsg["type"] = msgtype
	outmsg["schema_version"] = 2

	outmsg["md5"] = getString(inmsg, "md5", "")
	outmsg["size"] = getNumber(inmsg, "size", json.Number("0"))
	outmsg["compressed_size"] = getNumber(inmsg, "compressed_size", json.Number("0"))
	outmsg["node_id"] = getNumber(inmsg, "node_id", json.Number("0"))
	outmsg["file_path"] = getString(inmsg, "file_path", "")

	outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))
	outmsgs := make([]map[string]interface{}, 0, 1)
	outmsgs = append(outmsgs, outmsg)
	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) binaryinfoObserved(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsg := make(map[string]interface{})

	// message metadata
	outmsg["type"] = msgtype
	outmsg["schema_version"] = 2

	outmsg["md5"] = getString(inmsg, "md5", "")

	if msgtype == "binaryinfo.host.observed" {
		outmsg["hostname"] = getString(inmsg, "hostname", "")
		outmsg["sensor_id"] = getNumber(inmsg, "sensor_id", json.Number("0"))
	} else if msgtype == "binaryinfo.group.observed" {
		outmsg["group"] = getString(inmsg, "group", "")
	}

	for _, k := range []string{"scores", "watchlists"} {
		if val, ok := inmsg[k]; ok {
			if objval, ok := val.(map[string]interface{}); ok {
				outmsg[k] = deepcopy.Iface(objval).(map[string]interface{})
			} else {
				outmsg[k] = make(map[string]interface{})
			}
		} else {
			outmsg[k] = make(map[string]interface{})
		}
	}

	outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))
	outmsgs := make([]map[string]interface{}, 0, 1)
	outmsgs = append(outmsgs, outmsg)
	return outmsgs, nil
}

// ProcessJSON will take an incoming message and create a set of outgoing key/value
// pairs ready for the appropriate output function
func (jsp *JsonMessageProcessor) ProcessJSON(routingKey string, indata []byte) ([]map[string]interface{}, error) {
	var msg map[string]interface{}

	decoder := json.NewDecoder(bytes.NewReader(indata))

	// Ensure that we decode numbers in the JSON as integers and *not* float64s
	decoder.UseNumber()

	if err := decoder.Decode(&msg); err != nil {
		return nil, err
	}

	return jsp.ProcessJSONMessage(msg, routingKey)
}

func (jsp *JsonMessageProcessor) alertWatchlistHitBinary(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsg := make(map[string]interface{})

	// message metadata
	outmsg["type"] = msgtype
	outmsg["schema_version"] = 2

	err := copyAlertMetadata(msgtype, inmsg, outmsg)
	if err != nil {
		return nil, err
	}

	outmsg["md5"] = getString(inmsg, "md5", "")
	outmsg["digsig_result"] = getString(inmsg, "digsig_result", "(unknown)")

	if val, ok := inmsg["observed_filename"]; ok {
		if objval, ok := val.(map[string]interface{}); ok {
			outmsg["observed_filename"] = deepcopy.Iface(objval).(map[string]interface{})
		} else {
			outmsg["observed_filename"] = make(map[string]interface{})
		}
	} else {
		outmsg["observed_filename"] = make(map[string]interface{})
	}

	hostnames := make([]string, 0, 1)
	hostname := getString(inmsg, "hostname", "")
	if hostname != "" {
		hostnames = append(hostnames, hostname)
	}
	if val, ok := inmsg["other_hostnames"]; ok {
		if objval, ok := val.([]string); ok {
			for _, hostname = range objval {
				hostnames = append(hostnames, hostname)
			}
		}
	}
	outmsg["hostnames"] = hostnames

	outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))
	outmsg["created_time"] = getString(inmsg, "created_time", "")

	outmsgs := make([]map[string]interface{}, 0, 1)
	outmsgs = append(outmsgs, outmsg)
	return outmsgs, nil
}

func (jsp *JsonMessageProcessor) alertWatchlistHitHost(msgtype string, inmsg map[string]interface{}) ([]map[string]interface{}, error) {
	outmsg := make(map[string]interface{})

	// message metadata
	outmsg["type"] = msgtype
	outmsg["schema_version"] = 2

	err := copyAlertMetadata(msgtype, inmsg, outmsg)
	if err != nil {
		return nil, err
	}

	outmsg["event_timestamp"] = getNumber(inmsg, "event_timestamp", json.Number("0"))
	outmsg["created_time"] = getString(inmsg, "created_time", "")

	// sensor metadata includes everything but host_type, comms_ip, and interface_ip
	outmsg["sensor_id"] = getNumber(inmsg, "sensor_id", json.Number("0"))
	outmsg["hostname"] = getString(inmsg, "hostname", "")
	outmsg["group"] = getString(inmsg, "group", "")
	outmsg["os_type"] = getString(inmsg, "os_type", "")
	
	outmsgs := make([]map[string]interface{}, 0, 1)
	outmsgs = append(outmsgs, outmsg)
	return outmsgs, nil
}

func NewJSONProcessor(newConfig Config) *JsonMessageProcessor {
	jmp := new(JsonMessageProcessor)
	jmp.DebugFlag = newConfig.DebugFlag
	jmp.DebugStore = newConfig.DebugStore
	jmp.EventMap = deepcopy.Iface(newConfig.EventMap).(map[string]bool)
	jmp.CbServerURL = newConfig.CbServerURL
	jmp.CbAPI = newConfig.CbAPI

	// create message handlers
	jmp.messageHandlers = make(map[string]JSONMessageHandlerFunc)

	jmp.messageHandlers["watchlist.hit.process"] = jmp.watchlistHitProcess
	jmp.messageHandlers["watchlist.storage.hit.process"] = jmp.watchlistStorageHitProcess
	jmp.messageHandlers["watchlist.hit.binary"] = jmp.watchlistHitBinary
	jmp.messageHandlers["watchlist.storage.hit.binary"] = jmp.watchlistStorageHitBinary

	jmp.messageHandlers["feed.ingress.hit.process"] = jmp.feedIngressHitProcess
	jmp.messageHandlers["feed.storage.hit.process"] = jmp.feedStorageHitProcess
	jmp.messageHandlers["feed.query.hit.process"] = jmp.feedStorageHitProcess

	jmp.messageHandlers["alert.watchlist.hit.ingress.process"] = jmp.alertWatchlistHitProcess
	jmp.messageHandlers["alert.watchlist.hit.query.process"] = jmp.alertWatchlistHitProcess
	jmp.messageHandlers["alert.watchlist.hit.ingress.binary"] = jmp.alertWatchlistHitBinary
	jmp.messageHandlers["alert.watchlist.hit.query.binary"] = jmp.alertWatchlistHitBinary
	jmp.messageHandlers["alert.watchlist.hit.ingress.host"] = jmp.alertWatchlistHitHost

	jmp.messageHandlers["binarystore.file.added"] = jmp.binarystoreFileAdded
	jmp.messageHandlers["binaryinfo.observed"] = jmp.binaryinfoObserved
	jmp.messageHandlers["binaryinfo.host.observed"] = jmp.binaryinfoObserved
	jmp.messageHandlers["binaryinfo.group.observed"] = jmp.binaryinfoObserved

	return jmp
}
