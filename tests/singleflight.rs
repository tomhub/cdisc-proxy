    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };

    use axum::{Router, routing::get};
    use axum::body::to_bytes;
    use tower::util::ServiceExt;
    use http::Request;
    use tokio::task::JoinSet;

    use cdisc_proxy::{
        ProxyServer,
        AppConfig,
        ServerConfig,
        CdiscConfig,
        CacheConfig,
        L1Config,
        L2Config,
        SchedulerConfig,
    };

    fn test_config(upstream_url: &str) -> AppConfig {
        AppConfig::new(
            ServerConfig::new(3000, vec!["127.0.0.1".into()], None),
            CdiscConfig::new(upstream_url, "TESTKEY"),
            CacheConfig::new(
                L1Config::new("redis", "redis://127.0.0.1:6379", "60s"),
                L2Config::new(
                    Some("./test_sled".into()),
                    None,
                    "60s",
                    false,
                    "60s",
                ),
            ),
            SchedulerConfig::new(false, "60s"),
        )
    }


    #[tokio::test(flavor = "multi_thread", worker_threads = 8)]
    async fn test_singleflight_under_load() {
        let _ = std::fs::remove_dir_all("./test_sled");
        let hit_counter = Arc::new(AtomicUsize::new(0));

        let run_id = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_millis();
        let path = format!("/mdr/sdtm/products_{}", run_id);
        let api_path = format!("/api{}", path);

        let counter_clone = hit_counter.clone();
        let upstream_app = Router::new().route(
            &path,
            get({
                let counter = counter_clone.clone();
                move || async move {
                    counter.fetch_add(1, Ordering::SeqCst);
                    tokio::time::sleep(std::time::Duration::from_millis(300)).await;
                    (
                        axum::http::StatusCode::OK,
                        [("Content-Type", "application/json")],
                        r#"{"ok":true,"value":123}"#
                    )
                }
            }),
        );

        


        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let upstream_addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, upstream_app).await.unwrap();
        });

        tokio::time::sleep(std::time::Duration::from_millis(100)).await;

        let config = test_config(&format!("http://{}", upstream_addr));
        let server = ProxyServer::new(config).await.unwrap();

        let proxy_app = Router::new()
            .route("/api/{*path}", get(ProxyServer::handle_proxy))
            .with_state(server.clone());




        let mut set = JoinSet::new();
        for _ in 0..38000 {
            let app = proxy_app.clone();
            let uri = api_path.clone();
            set.spawn(async move {
                let req = Request::builder()
                    .uri(&uri)
                    .body(axum::body::Body::empty())
                    .unwrap();

                let resp = app.clone().oneshot(req).await.unwrap();
                let bytes = to_bytes(resp.into_body(), usize::MAX).await.unwrap();
                bytes.to_vec()
            });
        }

        let mut results = Vec::new();
        while let Some(res) = set.join_next().await {
            results.push(res.unwrap());
        }

        for r in &results[1..] {
            assert_eq!(r, &results[0]);
        }

        assert_eq!(hit_counter.load(Ordering::SeqCst), 1);
        assert_eq!(server.inflight_len(), 0);

        let _ = std::fs::remove_dir_all("./test_sled");
    }


