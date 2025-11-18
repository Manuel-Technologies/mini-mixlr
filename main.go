package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var (
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	rooms    = make(map[string]*Room)
	roomsMu  sync.RWMutex
)

type Room struct {
	Name        string
	Broadcaster *webrtc.PeerConnection
	Listeners   map[*webrtc.PeerConnection]bool
	mu          sync.RWMutex
}

func main() {
	http.HandleFunc("/create", createRoom)
	http.HandleFunc("/join/", joinRoom)
	log.Println("Mini-Mixlr backend running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func createRoom(w http.ResponseWriter, r *http.Request) {
	room := randomHex(5)
	roomsMu.Lock()
	rooms[room] = &Room{Name: room, Listeners: make(map[*webrtc.PeerConnection]bool)}
	roomsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"room": room, "url": "https://your-app.fly.dev/r/" + room})
}

func joinRoom(w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Path[len("/join/"):]
	roomsMu.RLock()
	room, ok := rooms[roomName]
	roomsMu.RUnlock()
	if !ok {
		http.Error(w, "Room not found", 404)
		return
	}

	ws, _ := upgrader.Upgrade(w, r, nil)
	defer ws.Close()

	config := webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}}
	pc, _ := webrtc.NewPeerConnection(config)
	defer pc.Close()

	isBroadcaster := r.URL.Query().Get("role") == "broadcaster"
	if isBroadcaster && room.Broadcaster != nil {
		ws.WriteJSON(map[string]string{"error": "already broadcasting"})
		return
	}

	if isBroadcaster {
		room.mu.Lock()
		room.Broadcaster = pc
		room.mu.Unlock()
	}

	handleSignaling(ws, pc, room, isBroadcaster)
}

func handleSignaling(ws *websocket.Conn, pc *webrtc.PeerConnection, room *Room, broadcaster bool) {
	// Full signaling logic stripped to minimum working version for mobile copy-paste
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			payload, _ := json.Marshal(map[string]any{"type": "candidate", "candidate": c.ToJSON()})
			ws.WriteMessage(1, payload)
		}
	})

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil { break }
		var m map[string]json.RawMessage
		json.Unmarshal(msg, &m)
		t := string(m["type"])
		if t == "offer" || t == "answer" {
			var sdp webrtc.SessionDescription
			json.Unmarshal(m["sdp"], &sdp)
			if t == "offer" {
				pc.SetRemoteDescription(sdp)
				ans, _ := pc.CreateAnswer(nil)
				pc.SetLocalDescription(ans)
				ws.WriteJSON(map[string]any{"type": "answer", "sdp": ans})
			} else {
				pc.SetRemoteDescription(sdp)
			}
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}