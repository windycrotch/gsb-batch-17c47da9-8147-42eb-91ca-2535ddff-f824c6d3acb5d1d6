/*
shottower
Copyright (C) 2022 Rémy Boulanouar

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
package openapi

import (
	"sync"
	"time"
)

type RenderQueue struct {
	// The id of the render task in UUID format.
	ID string `json:"id"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	Data     Edit `json:"data"`
	Status   RenderResponseStatus
	FileName string

	// Error holds the human readable failure reason when Status == Failed.
	Error string `json:"error,omitempty"`

	InternalStatus RenderResponseStatus
	FFMPEGCommand  FFMPEGCommand

	// stateMu protects the mutable lifecycle fields of this render. It is a
	// pointer so that RenderQueue value copies (e.g. callback payloads) never
	// copy a lock. A nil pointer is tolerated for zero valued instances.
	stateMu *sync.RWMutex `json:"-"`
}

// NewRenderQueue builds a trackable render task in the Queued state.
func NewRenderQueue(id string, data Edit) *RenderQueue {
	now := time.Now()
	return &RenderQueue{
		ID:             id,
		Created:        now,
		Updated:        now,
		Data:           data,
		Status:         Queued,
		InternalStatus: Queued,
		stateMu:        &sync.RWMutex{},
	}
}

func (r *RenderQueue) lockState() {
	if r.stateMu != nil {
		r.stateMu.Lock()
	}
}

func (r *RenderQueue) unlockState() {
	if r.stateMu != nil {
		r.stateMu.Unlock()
	}
}

func (r *RenderQueue) rLockState() {
	if r.stateMu != nil {
		r.stateMu.RLock()
	}
}

func (r *RenderQueue) rUnlockState() {
	if r.stateMu != nil {
		r.stateMu.RUnlock()
	}
}

// Transition updates both the public and internal status and bumps Updated.
func (r *RenderQueue) Transition(status RenderResponseStatus, internal RenderResponseStatus) {
	r.lockState()
	defer r.unlockState()
	r.Status = status
	r.InternalStatus = internal
	r.Updated = time.Now()
}

// SetStatus updates the public status and bumps Updated.
func (r *RenderQueue) SetStatus(status RenderResponseStatus) {
	r.lockState()
	defer r.unlockState()
	r.Status = status
	r.Updated = time.Now()
}

// SetInternalStatus updates the internal status without touching the public one.
func (r *RenderQueue) SetInternalStatus(internal RenderResponseStatus) {
	r.lockState()
	defer r.unlockState()
	r.InternalStatus = internal
}

// MarkUpdated bumps the Updated timestamp under the state lock.
func (r *RenderQueue) MarkUpdated() {
	r.lockState()
	defer r.unlockState()
	r.Updated = time.Now()
}

// Fail marks the render as Failed unless it has already reached Done.
// It returns true when the failure was recorded by this call.
func (r *RenderQueue) Fail(message string) bool {
	r.lockState()
	defer r.unlockState()
	if r.Status == Done {
		return false
	}
	r.Status = Failed
	r.InternalStatus = Failed
	if message != "" {
		r.Error = message
	}
	r.Updated = time.Now()
	return true
}

func (r *RenderQueue) GetStatus() RenderResponseStatus {
	r.rLockState()
	defer r.rUnlockState()
	return r.Status
}

func (r *RenderQueue) GetInternalStatus() RenderResponseStatus {
	r.rLockState()
	defer r.rUnlockState()
	return r.InternalStatus
}

func (r *RenderQueue) GetUpdated() time.Time {
	r.rLockState()
	defer r.rUnlockState()
	return r.Updated
}

func (r *RenderQueue) GetError() string {
	r.rLockState()
	defer r.rUnlockState()
	return r.Error
}

func (r *RenderQueue) GetFileName() string {
	r.rLockState()
	defer r.rUnlockState()
	return r.FileName
}

func (r *RenderQueue) SetFileName(name string) {
	r.lockState()
	defer r.unlockState()
	r.FileName = name
}

func (r *RenderQueue) GetFFMPEGCommand() FFMPEGCommand {
	r.rLockState()
	defer r.rUnlockState()
	return r.FFMPEGCommand
}

func (r *RenderQueue) SetFFMPEGCommand(cmd FFMPEGCommand) {
	r.lockState()
	defer r.unlockState()
	r.FFMPEGCommand = cmd
}

// Copy returns a value copy safe to hand to the callback retry loop.
func (r *RenderQueue) Copy() RenderQueue {
	r.rLockState()
	defer r.rUnlockState()
	return *r
}
