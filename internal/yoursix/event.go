// Package yoursix models the event payloads YourSix delivers to organization
// webhooks. See https://docs.yoursix.com/reference/events/structure.
package yoursix

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UserAgent is the User-Agent YourSix sends on every webhook request.
const UserAgent = "y6-platform"

// WebhookHost resolves (A record) to the IPv4 addresses webhooks are sent from.
const WebhookHost = "webhook.cloud.yoursix.com"

// Topics emitted by the platform.
const (
	TopicAlarmGroup = "alarmgroup"
	TopicDevice     = "device"
	TopicBarrier    = "barrier"
	TopicPeripheral = "peripheral"
	TopicSensor     = "sensor"
)

// Event is the base event model. EventData is kept as a generic map because
// its attributes depend on the topic and code, and individual events add
// vendor-specific attributes (e.g. videoSourceId on device:vmd).
type Event struct {
	EventID          string         `json:"eventId"`
	EventOccurred    string         `json:"eventOccurred"`
	EventCode        string         `json:"eventCode"`
	EventTopic       string         `json:"eventTopic"`
	EventDescription string         `json:"eventDescription"`
	EventData        map[string]any `json:"eventData"`
}

// Parse decodes and minimally validates a webhook body.
func Parse(body []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	ev.EventTopic = strings.TrimSpace(ev.EventTopic)
	ev.EventCode = strings.TrimSpace(ev.EventCode)
	if ev.EventTopic == "" || ev.EventCode == "" {
		return Event{}, errors.New("event is missing eventTopic or eventCode")
	}
	// Some documentation examples prefix the code with its topic
	// ("device:disconnected"); normalise to the bare code.
	if prefix := ev.EventTopic + ":"; strings.HasPrefix(ev.EventCode, prefix) {
		ev.EventCode = strings.TrimPrefix(ev.EventCode, prefix)
	}
	if ev.EventData == nil {
		ev.EventData = map[string]any{}
	}
	return ev, nil
}

// Key is the "topic:code" identifier, e.g. "device:vmd".
func (e Event) Key() string {
	return e.EventTopic + ":" + e.EventCode
}

// OccurredAt parses EventOccurred, falling back to the zero time.
func (e Event) OccurredAt() time.Time {
	t, err := time.Parse(time.RFC3339, e.EventOccurred)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Field looks up a value by name. Top-level attributes are addressed by their
// JSON name (eventCode, eventTopic, ...); anything else is read from
// eventData. A "data." prefix is accepted for clarity.
func (e Event) Field(name string) (any, bool) {
	switch name {
	case "eventId":
		return e.EventID, true
	case "eventOccurred":
		return e.EventOccurred, true
	case "eventCode":
		return e.EventCode, true
	case "eventTopic":
		return e.EventTopic, true
	case "eventDescription":
		return e.EventDescription, true
	}
	v, ok := e.EventData[strings.TrimPrefix(name, "data.")]
	return v, ok
}

// TemplateData is the value exposed to action templates.
//
//	{{ .event.eventCode }}  {{ .data.deviceName }}  {{ .rule }}
func (e Event) TemplateData(ruleName string) map[string]any {
	return map[string]any{
		"event": map[string]any{
			"eventId":          e.EventID,
			"eventOccurred":    e.EventOccurred,
			"eventCode":        e.EventCode,
			"eventTopic":       e.EventTopic,
			"eventDescription": e.EventDescription,
			"eventData":        e.EventData,
			"key":              e.Key(),
		},
		"data": e.EventData,
		"rule": ruleName,
		"now":  time.Now(),
	}
}

// Catalog lists every documented topic and code, used by the UI pickers.
// Source: https://docs.yoursix.com/reference/events/catalog
var Catalog = map[string][]string{
	TopicAlarmGroup: {"disarmed", "armed", "schedule"},
	TopicDevice: {
		"disconnected", "connected", "storage_disruption", "storage_disruption_end", "storage_healthy",
		"shock_detected", "tilt_detected", "video_tampering", "casing_open", "audio", "aoa", "vmd",
		"loitering_guard", "motion_guard", "fence_guard", "perimeter_defender", "pir", "radar_motion",
		"input_peripheral", "axis_imagehealth", "axis_imagehealth_end", "temperature_warning",
		"temperature_warning_end", "cpu_overutilization", "cpu_overutilization_end", "ram_overutilization",
		"ram_overutilization_end", "storage_full", "storage_full_end", "network_packetloss",
		"network_packetloss_end", "modbus_connected", "modbus_disconnected",
	},
	TopicBarrier: {
		"alarm", "alarm_end", "access_granted", "access_denied", "control_accessed",
		"control_locked", "control_unlocked", "mode_change",
	},
	TopicPeripheral: {
		"input_activated", "input_activated_end", "output_activated", "output_activated_end",
		"control_pulsed", "control_activated", "control_deactivated",
	},
	TopicSensor: {"outside_range", "outside_range_end"},
}
