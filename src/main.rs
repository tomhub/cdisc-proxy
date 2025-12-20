use reqwest::Method;
use serde::{Deserialize, Serialize};
use std::fs;
use std::time::Duration;
use tokio::time::sleep;

#[derive(Debug, Serialize, Deserialize, Clone)]
struct LastUpdated {
    #[serde(rename = "data-analysis")]
    data_analysis: String,
    #[serde(rename = "data-collection")]
    data_collection: String,
    #[serde(rename = "data-tabulation")]
    data_tabulation: String,
    integrated: String,
    overall: String,
    qrs: String,
    #[serde(default)]
    schemas: String,
    terminology: String,
}

#[derive(Debug)]
struct CdiscCacheInvalidator {
    api_url: String,
    varnish_url: String,
    state_file: String,
    check_interval: Duration,
    client: reqwest::Client,
}

impl CdiscCacheInvalidator {
    fn new(api_url: String, varnish_url: String, state_file: String, check_interval_secs: u64) -> Self {
        Self {
            api_url,
            varnish_url,
            state_file,
            check_interval: Duration::from_secs(check_interval_secs),
            client: reqwest::Client::builder()
                .timeout(Duration::from_secs(30))
                .build()
                .expect("Failed to create HTTP client"),
        }
    }

    async fn fetch_last_updated(&self) -> Result<LastUpdated, Box<dyn std::error::Error>> {
        let url = format!("{}/api/mdr/lastupdated", self.varnish_url);
        
        println!("Fetching lastupdated from cache: {}", url);
        
        let response = self.client.get(&url).send().await?;
        
        if !response.status().is_success() {
            return Err(format!("Cache returned status: {}", response.status()).into());
        }

        if let Some(cache_status) = response.headers().get("X-Cache") {
            println!("  Cache status: {}", cache_status.to_str().unwrap_or("unknown"));
        }

        let last_updated: LastUpdated = response.json().await?;
        Ok(last_updated)
    }


    fn load_previous_state(&self) -> Option<LastUpdated> {
        match fs::read_to_string(&self.state_file) {
            Ok(content) => match serde_json::from_str(&content) {
                Ok(s) => Some(s),
                Err(e) => {
                    eprintln!("Failed to parse state file {}: {}", self.state_file, e);
                    None
                }
            },
            Err(e) => {
                eprintln!("Failed to read state file {}: {}", self.state_file, e);
                None
            }
        }
    }

    fn save_state(&self, state: &LastUpdated) -> Result<(), Box<dyn std::error::Error>> {
        let json = serde_json::to_string_pretty(state)?;
        fs::write(&self.state_file, json)?;
        Ok(())
    }

    async fn invalidate_cache(&self, product_group: &str) -> Result<(), Box<dyn std::error::Error>> {
        let url = format!("{}/api/mdr/products/{}", self.varnish_url, product_group);

        println!("Invalidating cache for product group: {}", product_group);

        let ban_method = Method::from_bytes(b"BAN")?;

        let response = self.client
            .request(ban_method, &url)
            .header("X-Ban-Product", product_group)
            .send()
            .await?;

        let status = response.status();

        if status.is_success() {
            println!("✓ Successfully invalidated cache for: {}", product_group);
        } else {
            let body = response.text().await.unwrap_or_default();
            eprintln!(
                "✗ Failed to invalidate cache for {}: {} - {}",
                product_group,
                status,
                body
            );
        }

        Ok(())
    }

    fn compare_and_get_changes(&self, old: &LastUpdated, new: &LastUpdated) -> Vec<String> {
        let mut changes = Vec::new();

        if old.data_analysis != new.data_analysis { changes.push("data-analysis".to_string()); }
        if old.data_collection != new.data_collection { changes.push("data-collection".to_string()); }
        if old.data_tabulation != new.data_tabulation { changes.push("data-tabulation".to_string()); }
        if old.integrated != new.integrated { changes.push("integrated".to_string()); }
        if old.qrs != new.qrs { changes.push("qrs".to_string()); }
        if old.terminology != new.terminology { changes.push("terminology".to_string()); }

        changes
    }

    pub async fn run(&self) -> Result<(), Box<dyn std::error::Error>> {
        println!("CDISC Cache Invalidator starting...");
        println!("API URL: {}", self.api_url);
        println!("Varnish URL: {}", self.varnish_url);
        println!("Check interval: {:?}", self.check_interval);
        println!("State file: {}", self.state_file);
        println!("---");

        loop {
            match self.fetch_last_updated().await {
                Ok(current) => {
                    println!("\n[{}] Fetched last updated data", chrono::Local::now().format("%Y-%m-%d %H:%M:%S"));
                    
                    if let Some(previous) = self.load_previous_state() {
                        let changes = self.compare_and_get_changes(&previous, &current);
                        
                        if !changes.is_empty() {
                            println!("Changes detected in {} product group(s):", changes.len());
                            
                            for product_group in changes {
                                println!("  - {} (old: {:?}, new: {:?})", 
                                    product_group,
                                    self.get_product_date(&previous, &product_group),
                                    self.get_product_date(&current, &product_group)
                                );
                                
                                if let Err(e) = self.invalidate_cache(&product_group).await {
                                    eprintln!("Error invalidating cache for {}: {}", product_group, e);
                                }
                            }
                            
                            if let Err(e) = self.save_state(&current) {
                                eprintln!("Error saving state: {}", e);
                            } else {
                                println!("State saved successfully");
                            }
                        } else {
                            println!("No changes detected");
                        }
                    } else {
                        println!("No previous state found, initializing...");
                        let _ = self.save_state(&current);
                    }
                }
                Err(e) => eprintln!("Error fetching last updated: {}", e),
            }
            sleep(self.check_interval).await;
        }
    }

    fn get_product_date<'a>(&self, state: &'a LastUpdated, product: &str) -> &'a str {
        match product {
            "data-analysis" => &state.data_analysis,
            "data-collection" => &state.data_collection,
            "data-tabulation" => &state.data_tabulation,
            "integrated" => &state.integrated,
            "qrs" => &state.qrs,
            "terminology" => &state.terminology,
            _ => "",
        }
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let api_url = std::env::var("CDISC_API_URL").unwrap_or_else(|_| "https://library.cdisc.org".to_string());
    let varnish_url = std::env::var("VARNISH_URL").unwrap_or_else(|_| "http://localhost:6081".to_string());
    let state_file = std::env::var("STATE_FILE").unwrap_or_else(|_| "./cdisc_cache_state.json".to_string());
    let check_interval = std::env::var("CHECK_INTERVAL_SECS").unwrap_or_else(|_| "300".to_string()).parse::<u64>()?;

    let invalidator = CdiscCacheInvalidator::new(api_url, varnish_url, state_file, check_interval);
    invalidator.run().await
}
