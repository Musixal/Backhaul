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

4. [FAQ](#faq)
5. [License](#license)

---

## Introduction

This project offers a robust reverse tunneling solution to overcome NAT and firewall restrictions, supporting various transport protocols. It’s engineered for high efficiency and concurrency.

## Installation

1. **Download** the latest release from the [GitHub releases page](https://github.com/musixal/backhaul/releases).
2. **Extract** the archive and navigate to the extracted directory.
3. **Run** the executable as needed. You can also build from source if preferred.

## Usage

The main executable for this project is `backhaul`. It requires a TOML configuration file for both the server and client components.

### Configuration Options

To start using the solution, you'll need to configure both server and client components. Here’s how to set up basic configurations:

* **Server Configuration**

   Create a configuration file named `config.toml`:

    ```toml
    [server]
    bind_addr = "0.0.0.0:3080" # Address and port for the server to listen (mandatory).
    transport = "tcp"          # Protocol ("tcp", "tcpmux", or "ws", optional, default: "tcp").
    token = "your_token"       # Authentication token (optional).
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
   backhaul -c config.toml
   ```
* **Client Configuration**

   Create a configuration file named `config.toml` for the client:
   ```toml
   [client]
   remote_addr = "0.0.0.0:3080" # Server address and port (mandatory).
   transport = "tcp"            # Protocol ("tcp", "tcpmux", or "ws", optional, default: "tcp").
   token = "your_token"         # Authentication token (optional).
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
   backhaul -c config.toml
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
   [server]
   bind_addr = "0.0.0.0:3080"
   transport = "tcp"
   channel_size = 2048
   connection_pool = 8
   ```
* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "tcp"
   ```
* **Details**:

   `channel_size`: Adjust for high traffic. Determines how many connections the server will handle concurrently.

   `connection_pool`: Set the number of pre-established connections for better throughput.

#### TCP Multiplexing Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:3080"
   transport = "tcpmux"
   mux_session = 10
   ```
* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "tcpmux"
   mux_session = 10
   ```
* **Details**:

   `mux_session`: Number of multiplexed sessions. Increase this if you need to handle more simultaneous sessions over a single connection.

#### WebSocket Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:3080"
   transport = "ws"
   channel_size = 2048
   ```

* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "ws"
   ```

* **Details**:

   `channel_size`: Same as TCP, determines the maximum number of concurrent connections.


## FAQ

**Q: How do I decide which transport protocol to use?**

* `tcp`: Use if you need straightforward TCP connections.
* `tcpmux`: Use if you need to handle multiple sessions over a single connection.
* `ws`: Use if you need to traverse HTTP-based firewalls or proxies.

**Q: How can I optimize performance for high traffic?**

Increase the `channel_size` for TCP and WebSocket, and configure an appropriate `connection_pool` for TCP. For tcpmux, adjust the `mux_session` parameter.


## License

This project is licensed under the MIT License. See the LICENSE file for details.

## Donation
...