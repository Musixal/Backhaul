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
      - [Secure WebSocket Configuration](#secure-websocket-configuration)
      - [WS Multiplexing Configuration](#ws-multiplexing-configuration)
      - [WSS Multiplexing Configuration](#wss-multiplexing-configuration)
4. [Generating a Self-Signed TLS Certificate with OpenSSL](#generating-a-self-signed-tls-certificate-with-openssl)
5. [Running backhaul as a service](#running-backhaul-as-a-service)
6. [FAQ](#faq)
7. [License](#license)
8. [Donation](#donation)

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
    bind_addr = "0.0.0.0:3080"    # Address and port for the server to listen on (mandatory).
    transport = "tcp"             # Protocol to use ("tcp", "tcpmux", or "ws", mandatory).
    token = "your_token"          # Authentication token for secure communication (optional).
    keepalive_period = 20         # Interval in seconds to send keep-alive packets.(optional, default: 20 seconds)
    nodelay = false               # Enable TCP_NODELAY (optional, default: false).
    channel_size = 2048           # Tunnel channel size. Excess connections are discarded. Only for tcp and ws mode (optional, default: 2048).
    connection_pool = 8           # Number of pre-established connections. Only for tcp and ws mode (optional, default: 8).
    log_level = "info"            # Log level ("panic", "fatal", "error", "warn", "info", "debug", "trace", optional, default: "info").
    heartbeat = 20                # In seconds. Ping interval for tunnel stability. Min: 1s. Not used in TcpMux. (Optional, default: 20s)
    mux_session = 1               # Number of mux sessions for tcpmux. (optional, default: 1).
    mux_version = 1               # TCPMux protocol version (1 or 2). Version 2 may have extra features. (optional)
    mux_framesize = 32768         # 32 KB. The maximum size of a frame that can be sent over a connection. (optional)
    mux_recievebuffer = 4194304   # 4 MB. The maximum buffer size for incoming data per connection. (optional)
    mux_streambuffer = 65536      # 256 KB. The maximum buffer size per individual stream within a connection. (optional)
    sniffer = false               # Enable or disable network sniffing for monitoring data. (optional, default false)
    web_port = 2060               # Port number for the web interface or monitoring interface. (optional, default 2060).
    sniffer_log = "/root/backhaul.json" # Filename used to store network traffic and usage data logs. (optional, default backhaul.json)
    tls_cert = "/root/server.crt" # Path to the TLS certificate file for wss. (mandatory).
    tls_key = "/root/server.key"  # Path to the TLS private key file for wss.(mandatory).

    ports = [ # Local to remote port mapping in this format LocalPort=RemotePort (mandatory).
        "4000=5201", # Bind to all local ip addresses.
        "127.0.0.1:4001=5201", # Bind to specific local address.
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
   remote_addr = "0.0.0.0:3080"  # Server address and port (mandatory).
   transport = "tcp"             # Protocol to use ("tcp", "tcpmux", or "ws", mandatory).
   token = "your_token"          # Authentication token for secure communication (optional).
   keepalive_period = 20         # Interval in seconds to send keep-alive packets. (optional, default: 20 seconds)
   nodelay = false               # Use TCP_NODELAY (optional, default: false).
   retry_interval = 1            # Retry interval in seconds (optional, default: 1).
   log_level = "info"            # Log level ("panic", "fatal", "error", "warn", "info", "debug", "trace", optional, default: "info").
   mux_session = 1               # Number of mux sessions for tcpmux. (optional, default: 1).
   mux_version = 1               # TCPMux protocol version (1 or 2). Version 2 may have extra features. (optional)
   mux_framesize = 32768         # 32 KB. The maximum size of a frame that can be sent over a connection. (optional)
   mux_recievebuffer = 4194304   # 4 MB. The maximum buffer size for incoming data per connection. (optional)
   mux_streambuffer = 65536      # 256 KB. The maximum buffer size per individual stream within a connection. (optional)
   sniffer = false               # Enable or disable network sniffing for monitoring data. (optional, default false)
   web_port = 2060               # Port number for the web interface or monitoring interface. (optional, default 2060).
   sniffer_log = "/root/backhaul.json" # Filename used to store network traffic and usage data logs. (optional, default backhaul.json)

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
   [server]
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
   bind_addr = "0.0.0.0:8080"
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
   remote_addr = "0.0.0.0:8080"
   transport = "ws"
   token = "your_token" 
   nodelay = true 
   ```

* **Details**:

   * Refer to TCP configuration for more information.

#### Secure WebSocket Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:8443"
   transport = "wss"
   token = "your_token" 
   channel_size = 2048
   connection_pool = 8
   nodelay = true 
   tls_cert = "/root/server.crt"      
   tls_key = "/root/server.key"

   ports = []
   ```

* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:8443"
   transport = "wss"
   token = "your_token" 
   nodelay = true 
   ```

* **Details**:

   * Refer to the next section for instructions on generating `tls_cert` and `tls_key`.


#### WS Multiplexing Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:3080"
   transport = "wsmux"
   token = "your_token" 
   mux_session = 1
   nodelay = true 
   ports = []
   ```
* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:3080"
   transport = "wsmux"
   token = "your_token" 
   mux_session = 1
   nodelay = true 
   ```

#### WSS Multiplexing Configuration
* **Server**:

   ```toml
   [server]
   bind_addr = "0.0.0.0:443"
   transport = "wssmux"
   token = "your_token" 
   mux_session = 1
   nodelay = true 
   tls_cert = "/root/server.crt"      
   tls_key = "/root/server.key"
   ports = []
   ```
* **Client**:

   ```toml
   [client]
   remote_addr = "0.0.0.0:443"
   transport = "wssmux"
   token = "your_token" 
   mux_session = 1
   nodelay = true 
   ```



## Generating a Self-Signed TLS Certificate with OpenSSL

To generate a TLS certificate and key, you can use tools like OpenSSL. Here’s a step-by-step guide on how to create a self-signed certificate and key using OpenSSL:

### Step 1: Install OpenSSL

If you don't already have OpenSSL installed, you can install it using your system's package manager.

- **On Ubuntu/Debian**:
  ```bash
  sudo apt-get install openssl
  ```
### Step 2: Generate a Private Key
To generate a 2048-bit RSA private key, run the following command:
  ```bash
openssl genpkey -algorithm RSA -out server.key -pkeyopt rsa_keygen_bits:2048
  ```
This will create a file named `server.key`, which is your private key.
### Step 3: Generate a Certificate Signing Request (CSR)

Create a Certificate Signing Request (CSR) using the private key. This CSR is used to generate the SSL certificate:
  ```bash
openssl req -new -key server.key -out server.csr
  ```

You will be prompted to enter information for the CSR. For the common name (CN), use the domain name or IP address where your server will be hosted. Example:
```
Country Name (2 letter code) [AU]:US
State or Province Name (full name) [Some-State]:California
Locality Name (eg, city) []:San Francisco
Organization Name (eg, company) [Internet Widgits Pty Ltd]:Your Company Name
Organizational Unit Name (eg, section) []:
Common Name (e.g. server FQDN or YOUR name) []:example.com
Email Address []:
```

### Step 4: Generate a Self-Signed Certificate

Use the CSR and private key to generate a self-signed certificate. Specify the validity period (in days):
  ```bash
openssl x509 -req -in server.csr -signkey server.key -out server.crt -days 365
  ```
This will generate a certificate named `server.crt`, valid for 365 days.
### Recap of the Files Generated:

* `server.key`: Your private key.
* `server.csr`: The certificate signing request (used to generate the certificate).
* `server.crt`: Your self-signed TLS certificate.

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
* `wss`: Use this for secure WebSocket connections that need to traverse HTTP-based firewalls or proxies. It encrypts data for added security, similar to WS but with encryption.



## License

This project is licensed under the AGPL-3.0 license. See the LICENSE file for details.

## Donation

   <a href="https://nowpayments.io/donation?api_key=6Z16MRY-AF14Y8T-J24TXVS-00RDKK7&source=lk_donation&medium=referral" target="_blank">
     <img src="https://nowpayments.io/images/embeds/donation-button-white.svg" alt="Crypto donation button by NOWPayments">
    </a>

## Stargazers over time
[![Stargazers over time](https://starchart.cc/Musixal/Backhaul.svg?variant=light)](https://starchart.cc/Musixal/Backhaul)
