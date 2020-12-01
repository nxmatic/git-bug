package wrappers

import "sync"

type RWMutex struct {
	self sync.RWMutex
}

func (rw *RWMutex) RLock() {
	//rw.self.RLock()
}

func (rw *RWMutex) RUnlock() {
	//rw.self.RUnlock()
}

func (rw *RWMutex) Lock() {
	//rw.self.Lock()
}

func (rw *RWMutex) Unlock() {
	//rw.self.Unlock()
}
