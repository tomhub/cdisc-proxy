use percent_encoding::percent_decode_str;
use url::form_urlencoded;

/// Build a fully canonical cache key:
/// - normalize path (trim, collapse slashes, remove trailing slash)
/// - percent-decode path
/// - normalize query parameters (decode, sort, re-encode)
/// - stable encoding for final key
/// - namespace prefix lowercased
pub fn canonical_cache_key(path_and_query: &str, ns: &str) -> String {
    // Split path and query
    let mut parts = path_and_query.splitn(2, '?');
    let raw_path = parts.next().unwrap_or("");
    let raw_query = parts.next();

    // Normalize path
    let mut path = raw_path.trim().replace("//", "/");
    if path.ends_with('/') && path != "/" {
        path.pop();
    }

    // Percent-decode path
    let path = percent_decode_str(&path)
        .decode_utf8_lossy()
        .to_string();

    // Normalize query parameters
    let canonical_query = raw_query.map(|q| {
        let mut params: Vec<(String, String)> = q
            .split('&')
            .filter(|kv| !kv.is_empty())
            .filter_map(|kv| {
                let mut kvp = kv.splitn(2, '=');
                let k = kvp.next()?;
                let v = kvp.next().unwrap_or("");

                Some((
                    percent_decode_str(k).decode_utf8_lossy().to_string(),
                    percent_decode_str(v).decode_utf8_lossy().to_string(),
                ))
            })
            .collect();

        // Sort lexicographically
        params.sort_by(|a, b| a.0.cmp(&b.0).then(a.1.cmp(&b.1)));

        // Re-encode in stable form
        form_urlencoded::Serializer::new(String::new())
            .extend_pairs(params)
            .finish()
    });

    match canonical_query {
        Some(q) if !q.is_empty() => {
            format!("cdisc:cache:{}:{}?{}", ns.to_lowercase(), path, q)
        }
        _ => format!("cdisc:cache:{}:{}", ns.to_lowercase(), path),
    }
}
