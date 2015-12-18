package installer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/cznic/ql"
	"github.com/flynn/flynn/pkg/random"
)

func (prompt *Prompt) Resolve(res *Prompt) {
	prompt.Resolved = true
	prompt.resChan <- res
}

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

func (sub *Subscription) SendEvents(i *Installer) {
	if sub.isLocked {
		return
	}
	sub.isLocked = true
	sub.sendEventsMtx.Lock()
	defer sub.sendEventsMtx.Unlock()
	sub.isLocked = false
	for _, event := range i.GetEventsSince(sub.LastEventID) {
		sub.LastEventID = event.ID
		sub.EventChan <- event
	}
}

func (i *Installer) Subscribe(eventChan chan *Event, lastEventID string) *Subscription {
	subscription := &Subscription{
		LastEventID: lastEventID,
		EventChan:   eventChan,
	}

	go subscription.SendEvents(i)

	go func() {
		i.subscribeMtx.Lock()
		defer i.subscribeMtx.Unlock()
		i.subscriptions = append(i.subscriptions, subscription)
	}()

	return subscription
}

func (i *Installer) Unsubscribe(sub *Subscription) {
	i.subscribeMtx.Lock()
	defer i.subscribeMtx.Unlock()

	subscriptions := make([]*Subscription, 0, len(i.subscriptions))
	for _, s := range i.subscriptions {
		if sub != s {
			subscriptions = append(subscriptions, s)
		}
	}
	i.subscriptions = subscriptions
}

func (i *Installer) processEvent(event *Event) bool {
	var err error
	if event.Type == "log" {
		if c, err := i.FindBaseCluster(event.ClusterID); err != nil || (err == nil && c.State == "running") {
			return false
		}
	}
	if event.Type == "new_cluster" || event.Type == "install_done" || event.Type == "cluster_update" {
		event.Cluster, err = i.FindBaseCluster(event.ClusterID)
		if err != nil {
			i.logger.Debug(fmt.Sprintf("GetEventsSince Error finding cluster %s: %s", event.ClusterID, err.Error()))
			return false
		}
	}
	switch event.ResourceType {
	case "":
	case "prompt":
		p := &Prompt{}
		if err := i.db.QueryRow(`SELECT ID, Type, Message, Yes, Input, Resolved FROM prompts WHERE ID == $1 AND DeletedAt IS NULL`, event.ResourceID).Scan(&p.ID, &p.Type, &p.Message, &p.Yes, &p.Input, &p.Resolved); err != nil {
			i.logger.Debug(fmt.Sprintf("GetEventsSince Prompt Scan Error: %s", err.Error()))
			return false
		}
		event.Resource = p
	case "credential":
		if event.Type == "new_credential" {
			creds := &Credential{}
			if err := i.db.QueryRow(`SELECT Type, Name, ID FROM credentials WHERE ID == $1 AND DeletedAt IS NULL`, event.ResourceID).Scan(&creds.Type, &creds.Name, &creds.ID); err != nil {
				if err != sql.ErrNoRows {
					i.logger.Debug(fmt.Sprintf("GetEventsSince Credential Scan Error: %s", err.Error()))
				}
				return false
			}
			event.Resource = creds
		}
	default:
		i.logger.Debug(fmt.Sprintf("GetEventsSince unsupported ResourceType \"%s\"", event.ResourceType))
	}
	return true
}

func (i *Installer) GetEventsSince(eventID string) []*Event {
	events := make([]*Event, 0, len(i.events))
	var ts time.Time
	if eventID != "" {
		nano, err := strconv.ParseInt(strings.TrimPrefix(eventID, "event-"), 10, 64)
		if err != nil {
			i.logger.Debug(fmt.Sprintf("Error parsing event id: %s", err.Error()))
		} else {
			ts = time.Unix(0, nano)
		}
	}

	priority := []string{"new_cluster", "new_credential", "cluster_state", "install_done", "prompt"}
	for _, eventType := range priority {
		for _, e := range i.events {
			if !e.Timestamp.After(ts) {
				continue
			}
			if e.Type == eventType && i.processEvent(e) {
				events = append(events, e)
			}
		}
	}

	isStringIn := func(str string, strs []string) bool {
		for _, s := range strs {
			if str == s {
				return true
			}
		}
		return false
	}

	for _, e := range i.events {
		if !e.Timestamp.After(ts) {
			continue
		}
		if isStringIn(e.Type, priority) {
			continue
		}
		if i.processEvent(e) {
			events = append(events, e)
		}
	}

	return events
}

func (i *Installer) SendEvent(event *Event) {
	event.Timestamp = time.Now()
	event.ID = fmt.Sprintf("event-%d", event.Timestamp.UnixNano())

	if event.Type == "prompt" {
		prompt, ok := event.Resource.(*Prompt)
		if !ok || prompt == nil {
			i.logger.Debug(fmt.Sprintf("SendEvent Error: Invalid prompt event: %v", event))
			return
		}
		event.ResourceType = "prompt"
		event.ResourceID = prompt.ID
	}

	if event.Type == "error" {
		i.logger.Error(fmt.Sprintf("Error: %s", event.Description))
	} else {
		i.logger.Info(fmt.Sprintf("Event: %s: %s", event.Type, event.Description))
	}

	err := i.dbInsertItem("events", event)
	if err != nil {
		i.logger.Debug(fmt.Sprintf("SendEvent dbInsertItem error: %s", err.Error()))
	}

	i.eventsMtx.Lock()
	i.events = append(i.events, event)
	i.eventsMtx.Unlock()

	for _, sub := range i.subscriptions {
		go sub.SendEvents(i)
	}
}

func (c *BaseCluster) findPrompt(id string) (*Prompt, error) {
	if c.pendingPrompt != nil && c.pendingPrompt.ID == id {
		return c.pendingPrompt, nil
	}
	return nil, errors.New("Prompt not found")
}

func (c *BaseCluster) sendPrompt(prompt *Prompt) *Prompt {
	c.pendingPrompt = prompt

	if err := c.installer.dbInsertItem("prompts", prompt); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("sendPrompt db insert error: %s", err.Error()))
	}

	c.sendEvent(&Event{
		Type:      "prompt",
		ClusterID: c.ID,
		Resource:  prompt,
	})

	res := <-prompt.resChan
	prompt.Resolved = true
	if err := c.dbUpdatePrompt(prompt); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("sendPrompt db update error: %s", err.Error()))
	}
	prompt.Yes = res.Yes
	prompt.Input = res.Input
	if err := c.dbUpdatePrompt(prompt); err != nil {
		c.installer.logger.Debug(fmt.Sprintf("sendPrompt db update error: %s", err.Error()))
	}

	c.sendEvent(&Event{
		Type:      "prompt",
		ClusterID: c.ID,
		Resource:  prompt,
	})

	return res
}

func (c *BaseCluster) dbUpdatePrompt(prompt *Prompt) error {
	c.installer.dbMtx.Lock()
	defer c.installer.dbMtx.Unlock()

	return c.installer.txExec(`UPDATE prompts SET Resolved = $1, Yes = $2, Input = $3 WHERE ID == $4`, prompt.Resolved, prompt.Yes, prompt.Input, prompt.ID)
}

func (i *Installer) dbInsertItem(tableName string, item interface{}) error {
	i.dbMtx.Lock()
	defer i.dbMtx.Unlock()

	fields, err := i.dbMarshalItem(tableName, item)
	if err != nil {
		return err
	}

	vStr := make([]string, 0, len(fields))
	for idx := range fields {
		vStr = append(vStr, fmt.Sprintf("$%d", idx+1))
	}
	list, err := ql.Compile(fmt.Sprintf(`
    INSERT INTO %s VALUES(%s);
	`, tableName, strings.Join(vStr, ", ")))
	if err != nil {
		return err
	}
	return i.txExec(list.String(), fields...)
}

func (c *BaseCluster) prompt(typ, msg string) *Prompt {
	if c.State != "starting" && c.State != "deleting" {
		return &Prompt{}
	}
	res := c.sendPrompt(&Prompt{
		ID:      random.Hex(16),
		Type:    typ,
		Message: msg,
		resChan: make(chan *Prompt),
		cluster: c,
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

func (c *BaseCluster) sendEvent(event *Event) {
	c.installer.SendEvent(event)
}

func (c *BaseCluster) SendLog(description string) {
	c.sendEvent(&Event{
		Type:        "log",
		ClusterID:   c.ID,
		Description: description,
	})
}

func (c *BaseCluster) SendError(err error) {
	c.sendEvent(&Event{
		Type:        "error",
		ClusterID:   c.ID,
		Description: err.Error(),
	})
}

func (c *BaseCluster) handleDone() {
	if c.State != "running" {
		return
	}
	c.sendEvent(&Event{
		Type:      "install_done",
		ClusterID: c.ID,
		Cluster:   c,
	})
	msg, err := c.DashboardLoginMsg()
	if err != nil {
		panic(err)
	}
	c.installer.logger.Info(msg)
}
