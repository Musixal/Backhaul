# Backhaul

Welcome to the **`Backhaul`** project! This project provides a high-performance reverse tunneling solution optimized for handling massive concurrent connections through NAT and firewalls. This README will guide you through setting up and configuring both server and client components, including details on different transport protocols.

---

## Table of Contents

1. [Introduction](#introduction)
2. [Installation](#installation)
3. [Usage](#usage)
   - [Configuration Options](#configuration-options)
   - [Detailed Configuration](#detailed-configuration)
      - [Transport Protocols](#transport-protocols)
      - [TCP Configuration](#tcp-configuration)
      - [TCP Multiplexing Configuration](#tcp-multiplexing-configuration)
      - [WebSocket Configuration](#websocket-configuration)
4. [Running backhaul as a service](#running-backhaul-as-a-service)
5. [FAQ](#faq)
6. [License](#license)
7. [Donation](#donation)

---

## Introduction

This project offers a robust reverse tunneling solution to overcome NAT and firewall restrictions, supporting various transport protocols. It’s engineered for high efficiency and concurrency.

## Installation

1. **Download** the latest release from the [GitHub releases page](https://github.com/musixal/backhaul/releases).
2. **Extract** the archive (adjust the `filename` if needed):  

   ```bash
   tar -xzf backhaul_linux_amd64.tar.gz
   ``` 
3. **Run** the executable:  

   ```bash
   ./backhaul
   ```
4. You can also build from source if preferred:  

   ```bash
   git clone https://github.com/musixal/backhaul.git
   cd backhaul
   go build
   ./backhaul
   ```

## Usage

The main executable for this project is `backhaul`. It requires a TOML configuration file for both the server and client components.

### Configuration Options

To start using the solution, you'll need to configure both server and client components. Here’s how to set up basic configurations:

* **Server Configuration**

   Create a configuration file named `config.toml`:

    ```toml
    [server]# Local, IRAN
    bind_addr = "0.0.0.0:3080" # Address and port for the server to listen (mandatory).
    transport = "tcp"          # Protocol ("tcp", "tcpmux", or "ws", optional, default: "tcp").
    token = "your_token"       # Authentication token (optional).
    keepalive_period = 20      # Specify keep-alive period in seconds. (optional, default: 20 seconds)
    nodelay = false            # Enable TCP_NODELAY (optional, default: false).
    channel_size = 2048        # Tunnel channel size. Excess connections are discarded. Only for tcp and ws mode (optional, default: 2048).
    connection_pool = 8        # Number of pre-established connections. Only for tcp mode (optional, default: 8).
    mux_session = 1            # Number of mux sessions for tcpmux. (optional, default: 1).
    log_level = "info"         # Log level ("panic", "fatal", "error", "warn", "info", "debug", "trace", optional, default: "info").

    ports = [ # Local to remote port mapping in this format LocalPort=RemotePort (mandatory).
        "4000=5201",
        "4001=5201",
    ]
    ```

   To start the `server`:

   ```sh
   ./backhaul -c config.toml
   ```
* **Client Configuration**

   Create a configuration file named `config.toml` for the client:
   ```toml
   [client]  # Behind NAT, firewall-blocked
   remote_addr = "0.0.0.0:3080" # Server address and port (mandatory).
   transport = "tcp"            # Protocol ("tcp", "tcpmux", or "ws", optional, default: "tcp").
   token = "your_token"         # Authentication token (optional).
   keepalive_period = 20        # Specify keep-alive period in seconds. (optional, default: 20 seconds)
   nodelay = false              # Use TCP_NODELAY (optional, default: false).
   retry_interval = 1           # Retry interval in seconds (optional, default: 1).
   log_level = "info"           # Log level ("panic", "fatal", "error", "warn", "info", "debug", "trace", optional, default: "info").
   mux_session = 1              # Number of mux sessions for tcpmux. (optional, default: 1).

   forwarder = [ # Forward incoming connection to another address. optional.
      "4000=IP:PORT",
      "4001=127.0.0.1:9090",
   ]
   ```

   To start the `client`:

   ```sh
   ./backhaul -c config.toml
   ```

### Detailed Configuration
#### Transport Protocols

You can configure the `server` and `client` to use different transport protocols based on your requirements:

   * **TCP (`tcp`)**: Basic TCP transport, suitable for most scenarios.
   * **TCP Multiplexing (`tcpmux`)**: Provides multiplexing capabilities to handle multiple sessions over a single connection.
   * **WebSocket (`ws`)**: Ideal for traversing HTTP-based firewalls and proxies.

#### TCP Configuration
* **Server**:

   ```toml
   [server] # 
   bind_addr = "0.0.0.0:3080"
   transport = "tcp"
   token = "your_token" 
   channel_size = 2048
   connection_pool = 8
   nodelay = true 
   ports = []
   ```
* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "tcp"
   token = "your_token" 
   nodelay = true 
   ```
* **Details**:

   `remote_addr`: The IPv4, IPv6, or domain address of the server to which the client connects.

   `token`: An authentication token used to securely validate and authenticate the connection between the client and server within the tunnel.

   `channel_size`: The queue size for forwarding packets from server to the client. If the limit is exceeded, packets will be dropped.

   `connection_pool`: Set the number of pre-established connections for better latency.
   
   `nodelay`: Refers to a TCP socket option (TCP_NODELAY) that improve the latency but decrease the bandwidth

#### TCP Multiplexing Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:3080"
   transport = "tcpmux"
   token = "your_token" 
   mux_session = 1
   nodelay = true 
   ports = []
   ```
* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "tcpmux"
   token = "your_token" 
   mux_session = 1
   nodelay = true 
   ```
* **Details**:

   `mux_session`: Number of multiplexed sessions. Increase this if you need to handle more simultaneous sessions over a single connection.
   
   * Refer to TCP configuration for more information.


#### WebSocket Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:3080"
   transport = "ws"
   token = "your_token" 
   channel_size = 2048
   connection_pool = 8
   nodelay = true 
   ports = []
   ```

* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "ws"
   token = "your_token" 
   nodelay = true 
   ```

* **Details**:

   * Refer to TCP configuration for more information.


## Running backhaul as a service

To create a service file for your backhaul project that ensures the service restarts automatically, you can use the following template for a systemd service file. Assuming your project runs a reverse tunnel and the main executable file is located in a certain path, here's a basic example:

1. Create the service file `/etc/systemd/system/backhaul.service`:

```ini
[Unit]
Description=Backhaul Reverse Tunnel Service
After=network.target

[Service]
Type=simple
ExecStart=/root/backhaul -c /root/config.toml
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```
2. After creating the service file, enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable backhaul.service
sudo systemctl start backhaul.service
```
3. To verify if the service is running:
```bash
sudo systemctl status backhaul.service
```
4. View the most recent log entries for the backhaul.service unit:
```bash
journalctl -u backhaul.service -e -f
```

## FAQ

**Q: How do I decide which transport protocol to use?**

* `tcp`: Use if you need straightforward TCP connections.
* `tcpmux`: Use if you need to handle multiple sessions over a single connection.
* `ws`: Use if you need to traverse HTTP-based firewalls or proxies.



## License

This project is licensed under the AGPL-3.0 license. See the LICENSE file for details.

## Donation

   <a href="https://nowpayments.io/donation?api_key=6Z16MRY-AF14Y8T-J24TXVS-00RDKK7&source=lk_donation&medium=referral" target="_blank">
     <img src="https://nowpayments.io/images/embeds/donation-button-white.svg" alt="Crypto donation button by NOWPayments">
    </a>