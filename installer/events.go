package installer

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/flynn/flynn/pkg/random"
)

func (prompt *Prompt) Resolve(res *Prompt) {
	prompt.Resolved = true
	prompt.resChan <- res
}

// Used by pkg/sse
func (event *Event) EventID() string {
	return event.ID
}

type Subscription struct {
	LastEventID string
	EventChan   chan *Event
	DoneChan    chan struct{}

	isLocked      bool
	sendEventsMtx sync.Mutex
}

func (sub *Subscription) SendEvents(d *Data) {
	if sub.isLocked {
		return
	}
	sub.isLocked = true
	sub.sendEventsMtx.Lock()
	defer sub.sendEventsMtx.Unlock()
	sub.isLocked = false
	events, err := d.FetchEventsSince(sub.LastEventID)
	if err != nil {
		panic(err)
	}
	for _, event := range events {
		sub.LastEventID = event.ID
		sub.EventChan <- event
	}
}

func (d *Data) SubscribeEvents(eventChan chan *Event, lastEventID string) *Subscription {
	sub := &Subscription{
		LastEventID: lastEventID,
		EventChan:   eventChan,
	}

	go sub.SendEvents(d)

	go func() {
		d.subscriptionsMtx.Lock()
		defer d.subscriptionsMtx.Unlock()
		d.subscriptions = append(d.subscriptions, sub)
	}()

	return sub
}

func (d *Data) UnsubscribeEvents(sub *Subscription) {
	d.subscriptionsMtx.Lock()
	defer d.subscriptionsMtx.Unlock()

	subscriptions := make([]*Subscription, 0, len(d.subscriptions))
	for _, s := range d.subscriptions {
		if sub != s {
			subscriptions = append(subscriptions, s)
		}
	}
	d.subscriptions = subscriptions
}

func (d *Data) MustSendEvent(event *Event) {
	if err := d.SendEvent(event); err != nil {
		panic(err)
	}
}

func (d *Data) SendEvent(event *Event) error {
	event.Timestamp = time.Now()
	event.ID = fmt.Sprintf("event-%d", event.Timestamp.UnixNano())

	if event.Type == "prompt" {
		prompt, ok := event.Resource.(*Prompt)
		if !ok || prompt == nil {
			return fmt.Errorf("SendEvent Error: Invalid prompt event: %#v", event)
		}
		event.ResourceType = "prompt"
		event.ResourceID = prompt.ID
	}

	// TODO(jvatic): These should be processed through a subscription
	if event.Type == "error" {
		d.logger.Error(fmt.Sprintf("Error: %s", event.Description))
	} else {
		d.logger.Info(fmt.Sprintf("Event: %s: %s", event.Type, event.Description))
	}

	err := d.PersistItem(event)
	if err != nil {
		return fmt.Errorf("SendEvent dbInsertItem error: %s", err.Error())
	}

	d.subscriptionsMtx.Lock()
	for _, sub := range d.subscriptions {
		go sub.SendEvents(d)
	}
	d.subscriptionsMtx.Unlock()
	return nil
}

func (d *Data) MustSendPrompt(prompt *Prompt) *Prompt {
	res, err := d.SendPrompt(prompt)
	if err != nil {
		panic(err)
	}
	return res
}

func (d *Data) SendPrompt(prompt *Prompt) (*Prompt, error) {
	if err := d.PersistItem(prompt); err != nil {
		return nil, fmt.Errorf("SendPrompt db insert error: %s", err.Error())
	}

	if err := d.SendEvent(&Event{
		Type:      "prompt",
		ClusterID: prompt.ClusterID,
		Resource:  prompt,
	}); err != nil {
		return nil, err
	}

	res := <-prompt.resChan
	prompt.Resolved = true
	prompt.Yes = res.Yes
	prompt.Input = res.Input
	if err := d.UpdatePrompt(prompt); err != nil {
		return nil, fmt.Errorf("SendPrompt db update error: %s", err.Error())
	}

	if err := d.SendEvent(&Event{
		Type:      "prompt",
		ClusterID: prompt.ClusterID,
		Resource:  prompt,
	}); err != nil {
		return nil, err
	}

	return res, nil
}

func (c *BaseCluster) MustSendEvent(event *Event) {
	event.ClusterID = c.ID
	c.data.MustSendEvent(event)
}

func (c *BaseCluster) prompt(typ, msg string) *Prompt {
	if c.State != "starting" && c.State != "deleting" {
		return &Prompt{}
	}
	res := c.data.MustSendPrompt(&Prompt{
		ID:        random.Hex(16),
		ClusterID: c.ID,
		Type:      typ,
		Message:   msg,
		resChan:   make(chan *Prompt),
		cluster:   c,
	})
	return res
}

func (c *BaseCluster) YesNoPrompt(msg string) bool {
	res := c.prompt("yes_no", msg)
	return res.Yes
}

type Choice struct {
	Message string         `json:"message"`
	Options []ChoiceOption `json:"options"`
}

type ChoiceOption struct {
	Type  int    `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (c *BaseCluster) ChoicePrompt(choice Choice) (string, error) {
	data, err := json.Marshal(choice)
	if err != nil {
		return "", err
	}
	res := c.prompt("choice", string(data))
	return res.Input, nil
}

func (c *BaseCluster) CredentialPrompt(msg string) string {
	res := c.prompt("credential", msg)
	return res.Input
}

func (c *BaseCluster) PromptInput(msg string) string {
	res := c.prompt("input", msg)
	return res.Input
}

func (c *BaseCluster) PromptProtectedInput(msg string) string {
	res := c.prompt("protected_input", msg)
	return res.Input
}

func (c *BaseCluster) PromptFileInput(msg string) string {
	res := c.prompt("file", msg)
	return res.Input
}

func (c *BaseCluster) SendLog(description string) {
	c.MustSendEvent(&Event{
		Type:        "log",
		Description: description,
	})
}

func (c *BaseCluster) SendError(err error) {
	c.MustSendEvent(&Event{
		Type:        "error",
		Description: err.Error(),
	})
}

func (c *BaseCluster) handleDone() {
	if c.State != "running" {
		return
	}
	c.MustSendEvent(&Event{
		Type:    "install_done",
		Cluster: c,
	})
	msg, err := c.DashboardLoginMsg()
	if err != nil {
		panic(err)
	}
	c.logger.Info(msg)
}
