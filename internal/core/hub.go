package core

import "sync"

// Hub раздаёт события дебатов подписчикам (SSE-потоки, long-poll ожидания).
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{} // debateID -> подписчики
}

// NewHub создаёт пустой хаб.
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[chan Event]struct{})}
}

// Subscribe подписывает на события дискуссии. Канал буферизован;
// медленный подписчик теряет события, а не блокирует сервис.
func (h *Hub) Subscribe(debateID string) chan Event {
	ch := make(chan Event, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[debateID] == nil {
		h.subs[debateID] = make(map[chan Event]struct{})
	}
	h.subs[debateID][ch] = struct{}{}
	return ch
}

// Unsubscribe отписывает канал.
func (h *Hub) Unsubscribe(debateID string, ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.subs[debateID]; set != nil {
		delete(set, ch)
		if len(set) == 0 {
			delete(h.subs, debateID)
		}
	}
}

// Publish рассылает событие всем подписчикам дискуссии.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[ev.DebateID] {
		select {
		case ch <- ev:
		default: // подписчик не успевает — событие для него пропускается
		}
	}
}
