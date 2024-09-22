
## Setup

* **Operating System**: macOS 15.0
* **Hardware**: MacBook Air (M1)

## Bandwidth Test

We measured the bandwidth using `iperf3`:

```bash
iperf3 -c 127.0.0.1
```

<img src="https://github.com/Musixal/Backhaul/blob/main/benchmark/charts/1.png?raw=true" alt="Bandwidth Test" width="500"/>

## Apache Benchmark (ab) Test

The Apache Benchmark test was conducted using **ApacheBench, Version 2.3**. The following command was executed:
```bash
ab -n 10000 -c <Concurrency_Level> <URL>
```

* **Total Requests**: 10,000
* **Concurrency Level**: <Concurrency_Level>

### Results:

* **Requests per Second**:
<img src="https://github.com/Musixal/Backhaul/blob/main/benchmark/charts/2.png?raw=true" alt="Requests per Second" width="500"/>

* **Time per Request (mean)**:
<img src="https://github.com/Musixal/Backhaul/blob/main/benchmark/charts/3.png?raw=true" alt="Time per Request" width="500"/>