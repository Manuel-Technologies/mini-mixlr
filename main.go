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
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true }, // Warning: Remove in production
	}
	rooms   = make(map[string]*Room)
	roomsMu sync.RWMutex
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
	roomID := randomHex(6)
	roomsMu.Lock()
	rooms[roomID] = &Room{
		Name:      roomID,
		Listeners: make(map[*webrtc.PeerConnection]bool),
	}
	roomsMu.Unlock()

	resp := map[string]string{
		"room": roomID,
		"url":  "https://your-app.fly.dev/r/" + roomID,
	}
	json.NewEncoder(w).Encode(resp)
}

func joinRoom(w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Path[len("/join/"):]
	roomsMu.RLock()
	room, exists := rooms[roomName]
	roomsMu.RUnlock()

	if !exists {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer ws.Close()

	// Shared WebRTC configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Println("PeerConnection error:", err)
		return
	}
	defer pc.Close()

	isBroadcaster := r.URL.Query().Get("role") == "broadcaster"

	if isBroadcaster {
		if room.Broadcaster != nil {
			ws.WriteJSON(map[string]string{"error": "Room already has a broadcaster"})
			return
		}

		// Add audio track for broadcaster
		_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
		if err != nil {
			log.Println("AddTransceiver error:", err)
			return
		}

		room.mu.Lock()
		room.Broadcaster = pc
		room.mu.Unlock()

		// When broadcaster sends a track → forward to all listeners
		pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			log.Printf("Broadcaster sent track: %s", track.Kind())

			// Forward this track to every listener
			room.mu.RLock()
			for listener := range room.Listeners {
				go forwardTrack(track, listener)
			}
			room.mu.RUnlock()

			// Keep reading from broadcaster track (required)
			for {
				_, err := track.ReadRTP()
				if err != nil {
					break
				}
			}
		})
	} else {
		// Listener: create receive-only track
		_, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
			log.Println("Listener AddTransceiver error:", err)
			return
		}

		room.mu.Lock()
		room.Listeners[pc] = true
		room.mu.Unlock()

		// Cleanup on close
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateFailed {
				room.mu.Lock()
				delete(room.Listeners, pc)
				room.mu.Unlock()
			}
		})
	}

	handleSignaling(ws, pc, room, isBroadcaster)
}

// Forward incoming track from broadcaster to a listener
func forwardTrack(remoteTrack *webrtc.TrackRemote, listenerPC *webrtc.PeerConnection) {
	// Create a local track with same codec
	localTrack, err := webrtc.NewTrackLocalStaticSample(
		remoteTrack.Codec().Capability,
		remoteTrack.ID(), remoteTrack.StreamID())
	if err != nil {
		return
	}

	// Add to listener's PeerConnection
	_, err = listenerPC.AddTrack(localTrack)
	if err != nil {
		return
	}

	// Forward packets
	rtpBuf := make([]byte, 1400)
	for {
		n, _, err := remoteTrack.ReadRTP()
		if err != nil {
			return
		}
		// Copy buffer safely
		copy(rtpBuf, remoteTrack.Payload())
		localTrack.WriteSample(webrtc.Sample{Data: rtpBuf[:n], Duration: remoteTrack.Duration()})
	}
}

func handleSignaling(ws *websocket.Conn, pc *webrtc.PeerConnection, room *Room, isBroadcaster bool) {
	// Send ICE candidates
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidate, _ := json.Marshal(map[string]any{
			"type":      "candidate",
			"candidate": c.ToJSON().Candidate,
			"sdpMid":    c.ToJSON().SDPMid,
			"sdpMLineIndex": c.ToJSON().SDPMLineIndex,
		})
		ws.WriteMessage(websocket.TextMessage, candidate)
	})

	// Handle incoming messages
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Println("WebSocket read error:", err)
			break
		}

		var msgMap map[string]json.RawMessage
		if json.Unmarshal(msg, &msgMap) != nil {
			continue
		}

		msgType := string(msgMap["type"])

		switch msgType {
		case "offer":
			if !isBroadcaster {
				continue
			}
			var offer webrtc.SessionDescription
			if json.Unmarshal(msgMap["sdp"], &offer) != nil {
				continue
			}
			if err := pc.SetRemoteDescription(offer); err != nil {
				log.Println("SetRemoteDescription error:", err)
				continue
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Println("CreateAnswer error:", err)
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Println("SetLocalDescription error:", err)
			}
			ws.WriteJSON(map[string]any{"type": "answer", "sdp": answer})

		case "answer":
			if isBroadcaster {
				continue
			}
			var answer webrtc.SessionDescription
			if json.Unmarshal(msgMap["sdp"], &answer) != nil {
				continue
			}
			pc.SetRemoteDescription(answer)

		case "candidate":
			var candidate webrtc.ICECandidateInit
			if json.Unmarshal(msgMap["candidate"], &candidate) != nil {
				continue
			}
			pc.AddICECandidate(candidate)
		}
	}
}

func randomHex(n int) string {
	bytes := make([]byte, n)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
