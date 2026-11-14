// Command shiftcheck measures how long a gateway keeps sending traffic to a
// backend after it is killed. It sends a steady stream of requests, runs the
// kill command, and reports the time until the victim stops appearing in the
// X-Backend response header and requests succeed again.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/", "gateway URL")
	victim := flag.String("victim", "backend-3", "X-Backend value of the backend to kill")
	kill := flag.String("kill", "", "shell command that kills the victim")
	every := flag.Duration("every", 5*time.Millisecond, "request interval")
	stable := flag.Int("stable", 50, "consecutive clean responses that count as shifted")
	timeout := flag.Duration("timeout", 30*time.Second, "give up after this long")
	admin := flag.String("admin", "", "gateway admin URL; if set, also report when the gateway marks the victim down")
	victimURL := flag.String("victim-url", "", "substring of the victim's target URL in /admin/upstreams")
	flag.Parse()
	if *kill == "" {
		fmt.Fprintln(os.Stderr, "-kill is required")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 2 * time.Second}

	// Warm up: confirm the victim is serving traffic.
	seen := false
	for i := 0; i < 200 && !seen; i++ {
		if resp, err := client.Get(*url); err == nil {
			seen = resp.Header.Get("X-Backend") == *victim
			resp.Body.Close()
		}
	}
	if !seen {
		fmt.Fprintf(os.Stderr, "victim %q never served a request during warm-up\n", *victim)
		os.Exit(1)
	}

	if out, err := exec.Command("sh", "-c", *kill).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "kill failed: %v\n%s", err, out)
		os.Exit(1)
	}
	killed := time.Now()
	markedDown := make(chan time.Duration, 1)
	if *admin != "" && *victimURL != "" {
		go watchDown(client, *admin, *victimURL, killed, *timeout, markedDown)
	}

	var total, errs, victimHits, clean int
	var lastBad time.Duration
	for time.Since(killed) < *timeout {
		resp, err := client.Get(*url)
		total++
		bad := false
		if err != nil {
			errs++
			bad = true
		} else {
			if resp.StatusCode >= 500 {
				errs++
				bad = true
			}
			if resp.Header.Get("X-Backend") == *victim {
				victimHits++
				bad = true
			}
			resp.Body.Close()
		}
		if bad {
			clean = 0
			lastBad = time.Since(killed)
		} else if clean++; clean >= *stable {
			fmt.Printf("client-visible: last bad response %v after kill (requests=%d, errors=%d, victim hits after kill=%d)\n",
				lastBad.Round(time.Millisecond), total, errs, victimHits)
			if *admin != "" && *victimURL != "" {
				select {
				case d := <-markedDown:
					fmt.Printf("gateway marked victim down %v after kill\n", d.Round(time.Millisecond))
				case <-time.After(*timeout - time.Since(killed)):
					fmt.Println("gateway never marked victim down")
					os.Exit(1)
				}
			}
			return
		}
		time.Sleep(*every)
	}
	fmt.Fprintf(os.Stderr, "traffic did not shift within %v (errors=%d)\n", *timeout, errs)
	os.Exit(1)
}

type targetStatus struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Ejected bool   `json:"ejected"`
}

// watchDown polls the admin endpoint until the victim target is unhealthy or ejected.
func watchDown(c *http.Client, admin, victimURL string, killed time.Time, timeout time.Duration, out chan<- time.Duration) {
	for time.Since(killed) < timeout {
		resp, err := c.Get(strings.TrimSuffix(admin, "/") + "/admin/upstreams")
		if err == nil {
			var ups []struct {
				Targets []targetStatus `json:"targets"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&ups)
			resp.Body.Close()
			for _, u := range ups {
				for _, t := range u.Targets {
					if strings.Contains(t.URL, victimURL) && (!t.Healthy || t.Ejected) {
						out <- time.Since(killed)
						return
					}
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
}
