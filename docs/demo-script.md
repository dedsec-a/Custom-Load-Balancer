# 90-second demo video script

**Setup before recording:**
- Dashboard open at 1440×900, dark or light. Users at 0, algorithm **FCFS**, resources 2 cores / 5 DB.
- A second tab on `/experiments`, scrolled to the latest comparison result.
- Record the screen with sound (Windows: Win+Alt+R, or OBS). Speak slowly; cut pauses later.

| Time | Do | Say |
|---|---|---|
| 0:00–0:08 | Show the landing page hero. | "This is a load balancer I built in Go. Under overload, shortest-job-first cut short-request latency by 78%. Here's what that costs." |
| 0:08–0:20 | Open the dashboard. Click **50** users, then **150**. | "These are virtual users hitting three backends. At 150, the servers are full: throughput flattens at about 230 per second and the queue fills up." |
| 0:20–0:35 | Point at **p95 by endpoint**. Click **SJF**. | "Every request waits its turn, so the 20-millisecond requests wait behind the 100-millisecond ones. Switch to shortest-job-first, and /browse drops right away, while /search climbs. Same throughput: the scheduler only decides who waits." |
| 0:35–0:45 | Drag **SJF aging** to the right. | "Aging lets long jobs catch up the longer they wait. It's the dial between fast and fair." |
| 0:45–1:05 | Click **Least conn**, then **Simulate outage** on b2. | "Now the failure case. I'll kill backend two. It turns red within a second: passive health checks ejected it. The retried line jumps, and failed stays at zero, because each failed request was retried on another backend." |
| 1:05–1:15 | Wait for b2 to come back, or click **Recover now**. | "When it recovers, the active check brings it back automatically." |
| 1:15–1:30 | Switch to the experiments tab and show the bar chart and finding. | "And this isn't eyeballed. The experiment runner gives every algorithm the same seeded traffic, three times, and reports the median. That's where the 78% comes from. Link in the description." |

**After recording:** export at 1080p. Add the live link and GitHub link as text at the end. Upload unlisted to YouTube or as a LinkedIn video. A GIF of 0:20–0:35 works well at the top of the README.
