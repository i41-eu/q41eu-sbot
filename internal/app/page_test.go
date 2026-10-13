package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/wire"
)

// The page in a real browser: headless Chrome against a test server and a fake robot. Skipped
// without Chrome (CHROME=… names it).

// TestHeadTurnsTheWayTheControlsShow pins the direction of the head pad. A positive yaw turns
// the head to the robot's own left (counter-clockwise seen from above), a negative one to its
// right. On the page right means the robot's own right, as its camera shows it: the pad's button
// right of the middle and the → key send a lower yaw, the button on the left and ← a higher one,
// and the buttons' titles say which way.
func TestHeadTurnsTheWayTheControlsShow(t *testing.T) {
	_, srv := newTestApp(t, "")
	robot := dialWorker(t, srv, "stackchan-1", wire.RobotCapabilities{Model: "stackchan-cores3",
		Commands: []string{"look", "nod", "shake", "home", "camera"}}, nil)
	robot.next(wire.KindAccepted)
	var pc wire.PairCodeBody
	robot.next(wire.KindPairCode).Decode(&pc)
	robot.telemetry(map[string]float64{"head_yaw_deg": 0, "head_pitch_deg": 45})

	b := startChrome(t)
	b.open(pc.URL) // scanning the robot's QR code pairs this browser
	b.waitFor(`document.querySelector(".pad button[title]")`)
	robot.nextCommandNamed("camera", "the cockpit's camera view") // then the page stays as it is

	// look returns the yaw of the next look command, and reports it back as the robot does, so
	// the page's next step starts from there.
	look := func(what string) float64 {
		t.Helper()
		cmd := robot.nextCommandNamed("look", what)
		yaw, ok := cmd.Args["yaw"].(float64)
		if !ok {
			t.Fatalf("%s: look without a yaw: %v", what, cmd.Args)
		}
		robot.telemetry(map[string]float64{"head_yaw_deg": yaw, "head_pitch_deg": 45})
		return yaw
	}
	// settle waits for a held control's repeated steps and starts from the last one.
	yaw := 0.0
	settle := func() {
		t.Helper()
		for _, c := range robot.drainCommands() {
			if y, ok := c.Args["yaw"].(float64); ok && c.Command == "look" {
				yaw = y
				robot.telemetry(map[string]float64{"head_yaw_deg": yaw, "head_pitch_deg": 45})
			}
		}
	}
	// check fails unless the control on the side turned the head to the robot's same side.
	check := func(side, what string, got float64) {
		t.Helper()
		if side == "right" && got >= yaw || side == "left" && got <= yaw {
			t.Fatalf("%s on the %s sent yaw %v after %v: it turns the robot to its %s",
				what, side, got, yaw, map[bool]string{true: "left", false: "right"}[got > yaw])
		}
		yaw = got
	}

	// The pad's buttons beside its middle: on the right they turn the head to the robot's right
	// (yaw down), on the left to the robot's left (yaw up).
	for _, side := range []string{"right", "right", "left", "left", "left"} {
		settle()
		var btn struct {
			X, Y        float64
			Text, Title string
		}
		json.Unmarshal(b.eval(fmt.Sprintf(`(() => {
			const pad = [...document.querySelectorAll(".padbox")].find((p) => p.querySelector("h3").textContent.startsWith("Head")).querySelector(".pad");
			pad.scrollIntoView({ block: "center" });
			const p = pad.getBoundingClientRect(), cx = p.left + p.width / 2, cy = p.top + p.height / 2;
			const b = [...pad.querySelectorAll("button")].find((b) => {
				const r = b.getBoundingClientRect();
				return r.top < cy && cy < r.bottom && (%q === "right" ? r.left > cx : r.right < cx);
			});
			const r = b.getBoundingClientRect();
			return { x: r.left + r.width / 2, y: r.top + r.height / 2, text: b.textContent, title: b.title };
		})()`, side)), &btn)
		if !strings.Contains(btn.Title, "robot's "+side) {
			t.Fatalf("the head button on the %s (%s) has the title %q", side, btn.Text, btn.Title)
		}
		b.mouseClick(btn.X, btn.Y) // the pad acts on press and stops on release
		check(side, "the head button "+btn.Text, look("the head button "+btn.Text))
	}

	// The arrow keys: → like the button on the right, ← like the one on the left.
	for _, k := range []struct {
		side, key string
		code      int
	}{{"right", "ArrowRight", 39}, {"left", "ArrowLeft", 37}, {"left", "ArrowLeft", 37}} {
		settle()
		b.key(k.key, k.code)
		check(k.side, "the "+k.key+" key", look("the "+k.key+" key"))
	}
}

// --- the robot's side: the commands it receives -----------------------------------------------

// nextCommandNamed returns the next command with the name, skipping others.
func (w *worker) nextCommandNamed(name, what string) wire.RobotCommandBody {
	w.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-w.frames:
			var c wire.RobotCommandBody
			if f.Kind == wire.KindRobotCommand && f.Decode(&c) == nil && c.Command == name {
				return c
			}
		case <-deadline:
			w.t.Fatalf("%s: the robot got no %s command", what, name)
		}
	}
}

// drainCommands returns the commands received so far, after a pause (a held button repeats its
// step), and drops the other frames.
func (w *worker) drainCommands() []wire.RobotCommandBody {
	time.Sleep(300 * time.Millisecond)
	var cmds []wire.RobotCommandBody
	for {
		select {
		case f := <-w.frames:
			var c wire.RobotCommandBody
			if f.Kind == wire.KindRobotCommand && f.Decode(&c) == nil {
				cmds = append(cmds, c)
			}
		default:
			return cmds
		}
	}
}

// --- the browser --------------------------------------------------------------------------------

type chrome struct {
	t   *testing.T
	ws  *websocket.Conn
	mu  sync.Mutex
	id  int
	res map[int]chan json.RawMessage
}

func chromePath() string {
	if p := os.Getenv("CHROME"); p != "" {
		return p
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// startChrome runs a headless Chrome with one tab, controlled over the DevTools protocol.
func startChrome(t *testing.T) *chrome {
	t.Helper()
	bin := chromePath()
	if bin == "" {
		t.Skip("no Chrome (set CHROME)")
	}
	profile, err := os.MkdirTemp("", "sbot-page-chrome-") // not t.TempDir: Chrome's helpers may still write there at the end
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--remote-debugging-port=0", "--user-data-dir="+profile, "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM) // Chrome closes its helper processes too
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		os.RemoveAll(profile)
	})

	// Chrome writes the port it listens on to DevToolsActivePort.
	var port string
	for i := 0; port == ""; i++ {
		if f, err := os.Open(filepath.Join(profile, "DevToolsActivePort")); err == nil {
			s := bufio.NewScanner(f)
			if s.Scan() {
				port = strings.TrimSpace(s.Text())
			}
			f.Close()
		}
		if i > 150 {
			t.Fatal("Chrome did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var tab struct{ WebSocketDebuggerURL string }
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:"+port+"/json/new?about:blank", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&tab)
	resp.Body.Close()
	ws, _, err := websocket.DefaultDialer.Dial(tab.WebSocketDebuggerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	c := &chrome{t: t, ws: ws, res: map[int]chan json.RawMessage{}}
	go func() {
		for {
			var m struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if ws.ReadJSON(&m) != nil {
				return
			}
			c.mu.Lock()
			ch := c.res[m.ID]
			delete(c.res, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
		}
	}()
	c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 900, "height": 1200, "deviceScaleFactor": 1, "mobile": false})
	return c
}

func (c *chrome) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := c.id
	ch := make(chan json.RawMessage, 1)
	c.res[id] = ch
	err := c.ws.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	c.mu.Unlock()
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		c.t.Fatalf("%s: no answer", method)
		return nil
	}
}

// eval runs JavaScript in the page and returns its value as JSON.
func (c *chrome) eval(js string) json.RawMessage {
	c.t.Helper()
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	json.Unmarshal(c.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "awaitPromise": true}), &r)
	if r.ExceptionDetails != nil {
		c.t.Fatalf("in the page: %s", r.ExceptionDetails.Exception.Description)
	}
	return r.Result.Value
}

func (c *chrome) str(js string) string {
	var s string
	json.Unmarshal(c.eval(js), &s)
	return s
}

func (c *chrome) open(url string) {
	c.t.Helper()
	c.call("Page.navigate", map[string]any{"url": url})
	c.waitFor(`document.readyState === "complete"`)
}

// waitFor waits until the JavaScript condition is true (10 s).
func (c *chrome) waitFor(cond string) {
	c.t.Helper()
	for i := 0; i < 100; i++ {
		if string(c.eval("!!("+cond+")")) == "true" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("waiting for %s; the page says: %q", cond, c.str(`document.body.innerText.slice(0, 400)`))
}

// mouseClick presses and releases the left mouse button at a point of the viewport.
func (c *chrome) mouseClick(x, y float64) {
	c.t.Helper()
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		c.call("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1})
	}
}

// key presses and releases a key without text (an arrow key).
func (c *chrome) key(key string, code int) {
	c.t.Helper()
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		c.call("Input.dispatchKeyEvent", map[string]any{"type": typ, "key": key, "code": key,
			"windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code})
	}
}
