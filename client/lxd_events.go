package lxd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
)

// Event handling functions

// getEvents connects to the LXD monitoring interface
func (r *ProtocolLXD) getEvents(allProjects bool) (*EventListener, error) {
	// Prevent anything else from interacting with the listeners
	r.eventListenersLock.Lock()
	defer r.eventListenersLock.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	// Setup a new listener
	listener := EventListener{
		r:         r,
		ctx:       ctx,
		ctxCancel: cancel,
	}

	connInfo, _ := r.GetConnectionInfo()
	if connInfo.Project == "" {
		return nil, fmt.Errorf("Unexpected empty project in connection info")
	}

	if !allProjects {
		listener.projectName = connInfo.Project
	}

	// Initialise the connection event listener map if needed.
	if r.eventListeners == nil {
		r.eventListeners = make(map[string][]*EventListener)
	}

	// There is an existing Go routine for the required project filter, so just add another target.
	if r.eventListeners[listener.projectName] != nil {
		r.eventListeners[listener.projectName] = append(r.eventListeners[listener.projectName], &listener)
		return &listener, nil
	}

	// Setup a new connection with LXD
	var url string
	var err error
	if allProjects {
		url, err = r.setQueryAttributes("/events?all-projects=true")
	} else {
		url, err = r.setQueryAttributes("/events")
	}
	if err != nil {
		return nil, err
	}

	r.eventConn, err = r.websocket(url)
	if err != nil {
		return nil, err
	}

	// Initialize the event listener list if we were able to connect to the events websocket.
	r.eventListeners[listener.projectName] = []*EventListener{&listener}

	// Spawn a watcher that will close the websocket connection after all
	// listeners are gone.
	stopCh := make(chan struct{})
	go func() {
		for {
			select {
			case <-time.After(time.Minute):
			case <-r.chConnected:
			case <-stopCh:
			}

			r.eventListenersLock.Lock()
			if len(r.eventListeners) == 0 {
				// We don't need the connection anymore, disconnect
				r.eventConn.Close()

				r.eventListeners[listener.projectName] = nil
				r.eventListenersLock.Unlock()
				break
			}
			r.eventListenersLock.Unlock()
		}
	}()

	// Spawn the listener
	go func() {
		for {
			_, data, err := r.eventConn.ReadMessage()
			if err != nil {
				// Prevent anything else from interacting with the listeners
				r.eventListenersLock.Lock()
				defer r.eventListenersLock.Unlock()

				// Tell all the current listeners about the failure
				for _, listener := range r.eventListeners[listener.projectName] {
					listener.err = err
					listener.ctxCancel()
				}

				// And remove them all from the list
				r.eventListeners[listener.projectName] = nil

				r.eventConn.Close()
				close(stopCh)

				return
			}

			// Attempt to unpack the message
			event := api.Event{}
			err = json.Unmarshal(data, &event)
			if err != nil {
				continue
			}

			// Extract the message type
			if event.Type == "" {
				continue
			}

			// Send the message to all handlers
			r.eventListenersLock.Lock()
			for _, listener := range r.eventListeners[listener.projectName] {
				listener.targetsLock.Lock()
				for _, target := range listener.targets {
					if target.types != nil && !shared.StringInSlice(event.Type, target.types) {
						continue
					}

					go target.function(event)
				}
				listener.targetsLock.Unlock()
			}
			r.eventListenersLock.Unlock()
		}
	}()

	return &listener, nil
}

// GetEvents gets the events for the project defined on the client.
func (r *ProtocolLXD) GetEvents() (*EventListener, error) {
	return r.getEvents(false)
}

// GetEventsAllProjects gets events for all projects.
func (r *ProtocolLXD) GetEventsAllProjects() (*EventListener, error) {
	return r.getEvents(true)
}
