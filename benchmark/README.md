
## Setup

* **Operating System**: macOS 15.0
* **Hardware**: MacBook Air (M1)

## Bandwidth Test

We measured the bandwidth using `iperf3`:

```bash
iperf3 -c 127.0.0.1
```

![Bandwidth Test](benchmark/1.png)

## Apache Benchmark (ab) Test

The Apache Benchmark test was conducted using **ApacheBench, Version 2.3**. The following command was executed:
```bash
ab -n 10000 -c <Concurrency_Level> <URL>
```

* **Total Requests**: 10,000
* **Concurrency Level**: <Concurrency_Level>

### Results:

* Requests per Second:*
![Requests per Second](benchmark/2.png)

* Time per Request (mean):*
![Time per Request](benchmark/3.png)