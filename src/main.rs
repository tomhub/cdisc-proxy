use anyhow::{Context, Result};
use axum::{Router, routing::get};
use tower_http::timeout::TimeoutLayer;
use hyper::StatusCode;
use tokio::net::TcpListener;
use tokio::signal::unix::{signal, SignalKind};
use tokio::select;
use tracing::{info, error};

use std::net::SocketAddr;

use cdisc_proxy::{
    ProxyServer,
    AppConfig,
    validate_config,
    REQUEST_TIMEOUT,
};

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt::init();

    let config_path = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "/etc/conf.d/cdisc-proxy.conf".to_string());

    let config_data = tokio::fs::read_to_string(&config_path).await?;
    let config: AppConfig = serde_yaml::from_str(&config_data)?;

    validate_config(&config)?;

    let server = ProxyServer::new(config.clone()).await?;

    let app = Router::new()
        .route("/api/{*path}", get(ProxyServer::handle_proxy))
        .route("/health", get(ProxyServer::handle_health))
        .layer(TimeoutLayer::with_status_code(StatusCode::REQUEST_TIMEOUT, REQUEST_TIMEOUT))
        .with_state(server.clone());

    let listen_addrs = if config.server.listen.is_empty() {
        vec!["0.0.0.0".to_string()]
    } else {
        config.server.listen.clone()
    };

    let addr_str = format!("{}:{}", listen_addrs[0], config.server.port);
    let addr: SocketAddr = addr_str.parse().context("invalid listen address")?;

    info!("Listening on {}", addr);

    let listener = TcpListener::bind(addr).await.context("failed to bind to address")?;
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await
        .context("server failed")?;

    Ok(())
}

async fn shutdown_signal() {
    let mut sigterm = match signal(SignalKind::terminate()) {
        Ok(s) => s,
        Err(e) => {
            error!("failed to create SIGTERM handler: {:?}", e);
            return;
        }
    };
    let mut sigint = match signal(SignalKind::interrupt()) {
        Ok(s) => s,
        Err(e) => {
            error!("failed to create SIGINT handler: {:?}", e);
            return;
        }
    };

    select! {
        _ = sigterm.recv() => info!("Received SIGTERM"),
        _ = sigint.recv() => info!("Received SIGINT"),
    }
}
