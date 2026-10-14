package yoursix

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Sample builds a realistic payload for topic/code, mirroring the examples in
// the YourSix event catalog. Used for the UI's test button and dev tooling.
func Sample(topic, code string) Event {
	data := map[string]any{
		"siteId":           "182140d9-d99c-4dc8-ae14-0071a01d7e98",
		"siteName":         "HQ",
		"siteDescription":  "Company headquarters",
		"groupId":          "cf84e116-ee61-4bd8-b078-3d2fadfcc9ac",
		"groupName":        "West entrance",
		"groupDescription": "E1",
		"groupActive":      true,
		"groupState":       "schedule",
	}
	if topic != TopicAlarmGroup {
		data["deviceId"] = "0eeb5774-e88d-11ef-95f8-afbaa9403ab9"
		data["deviceName"] = "CAM-01"
		data["deviceDescription"] = "Camera 01"
	}
	switch topic {
	case TopicBarrier:
		data["barrierId"] = "5d0b1f7c-ee13-11ef-bc15-b33a54d715fa"
		data["barrierName"] = "Front door"
		data["barrierDescription"] = "Main entrance door"
	case TopicPeripheral:
		data["peripheralId"] = "6630e036-ee13-11ef-bc15-b33a54d715fa"
		data["peripheralName"] = "Input 1"
		data["peripheralDescription"] = "Door contact"
	case TopicSensor:
		data["sensorId"] = "7a41c2de-ee13-11ef-bc15-b33a54d715fa"
		data["sensorName"] = "Temperature"
		data["sensorDescription"] = "Server room temperature"
	case TopicDevice:
		switch code {
		case "vmd", "aoa", "motion_guard", "fence_guard", "loitering_guard":
			data["videoSourceId"] = "0"
		}
	}
	return Event{
		EventID:          newUUID(),
		EventOccurred:    time.Now().Format(time.RFC3339),
		EventTopic:       topic,
		EventCode:        code,
		EventDescription: fmt.Sprintf("Sample %s:%s event", topic, code),
		EventData:        data,
	}
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
