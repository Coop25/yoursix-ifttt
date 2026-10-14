package yoursix

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultPlatformURL is the YourSix web app that Matrix deeplinks open in.
const DefaultPlatformURL = "https://platform.yoursix.com"

// MatrixURL builds a deeplink that opens an event's video in the YourSix
// Matrix, playing back from when it occurred. See "Deeplinks" in
// https://docs.yoursix.com/reference/integration:
//
//	/matrix?deviceIds=DEVICE-ID_SOURCE-ID&timestamp=SECONDS
//	/matrix?alarmGroupId=GROUP-ID&timestamp=SECONDS
//
// The source suffix is the event's videoSourceId exactly as given. Without
// one the whole device is opened; without a device, the event's alarm
// group. It returns "" when the event has neither.
func MatrixURL(base, deviceID, videoSourceID, groupID string, at time.Time) string {
	q := url.Values{}
	switch {
	case deviceID != "":
		ids := deviceID
		if src := strings.TrimSpace(videoSourceID); src != "" {
			ids += "_" + src
		}
		q.Set("deviceIds", ids)
	case groupID != "":
		q.Set("alarmGroupId", groupID)
	default:
		return ""
	}
	if !at.IsZero() {
		// Seconds with millisecond precision, e.g. 1791996197.084.
		q.Set("timestamp", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))
	}
	if base == "" {
		base = DefaultPlatformURL
	}
	return strings.TrimRight(base, "/") + "/matrix?" + q.Encode()
}
