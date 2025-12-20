vcl 4.1;

import std;
import directors;

# CDISC API key - set this via environment variable or config file
# This will be injected into all upstream requests
include "/etc/varnish/api_key.vcl";

# Backend definition
backend default {
    .host = "library.cdisc.org";
    .port = "443";
    .connect_timeout = 10s;
    .first_byte_timeout = 60s;
    .between_bytes_timeout = 10s;
}

# ACL for purge requests (adjust IPs as needed)
acl purge_acl {
    "localhost";
    "127.0.0.1";
    "::1";
}

sub vcl_recv {
    # Handle PURGE requests for cache invalidation
    if (req.method == "PURGE") {
        if (!client.ip ~ purge_acl) {
            return (synth(405, "Not allowed"));
        }
        return (purge);
    }

    # Handle BAN requests for pattern-based invalidation
    if (req.method == "BAN") {
        if (!client.ip ~ purge_acl) {
            return (synth(405, "Not allowed"));
        }
        
        # Ban by product group pattern
        if (req.http.X-Ban-Product) {
            ban("obj.http.X-Product-Group == " + req.http.X-Ban-Product);
            return (synth(200, "Banned product group: " + req.http.X-Ban-Product));
        }
        
        return (synth(400, "Invalid BAN request"));
    }

    # Only cache GET and HEAD requests
    if (req.method != "GET" && req.method != "HEAD") {
        return (pass);
    }

    # Cache lastupdated endpoint for short time (configurable via header)
    # Default: 5 minutes
    if (req.url ~ "^/api/mdr/lastupdated") {
        # Allow override via X-Lastupdated-TTL header (in seconds)
        if (req.http.X-Lastupdated-TTL) {
            set req.http.X-Custom-TTL = req.http.X-Lastupdated-TTL;
        } else {
            set req.http.X-Custom-TTL = "300";
        }
    }

    # Normalize URL and remove tracking parameters
    set req.url = std.querysort(req.url);
    
    return (hash);
}

sub vcl_hash {
    hash_data(req.url);
    
    # Include Accept header in cache key for content negotiation
    if (req.http.Accept) {
        hash_data(req.http.Accept);
    }
    
    return (lookup);
}

sub vcl_backend_response {
    # Handle lastupdated endpoint caching
    if (bereq.url ~ "^/api/mdr/lastupdated") {
        if (bereq.http.X-Custom-TTL) {
            set beresp.ttl = std.duration(bereq.http.X-Custom-TTL + "s", 300s);
        } else {
            set beresp.ttl = 5m;
        }
        set beresp.grace = 1m;
        set beresp.http.Cache-Control = "public, max-age=300";
        return (deliver);
    }

    # Set product group header for selective invalidation
    if (bereq.url ~ "^/api/mdr/products/DataAnalysis" || bereq.url ~ "^/mdr/adam") {
        set beresp.http.X-Product-Group = "data-analysis";
    } elsif (bereq.url ~ "^/api/mdr/products/DataCollection" || bereq.url ~ "^/mdr/cdash") {
        set beresp.http.X-Product-Group = "data-collection";
    } elsif (bereq.url ~ "^/api/mdr/products/DataTabulation" || bereq.url ~ "^/mdr/sdtm" || bereq.url ~ "^/mdr/sendig") {
        set beresp.http.X-Product-Group = "data-tabulation";
    } elsif (bereq.url ~ "^/api/mdr/products/Integrated" || bereq.url ~ "^/mdr/integrated") {
        set beresp.http.X-Product-Group = "integrated";
    } elsif (bereq.url ~ "^/api/mdr/products/QrsInstrument" || bereq.url ~ "^/mdr/qrs") {
        set beresp.http.X-Product-Group = "qrs";
    } elsif (bereq.url ~ "^/api/mdr/products/Terminology" || bereq.url ~ "^/mdr/ct") {
        set beresp.http.X-Product-Group = "terminology";
    } elsif (bereq.url ~ "^/api/mdr/products") {
        set beresp.http.X-Product-Group = "products";
    }

    # Enable persistent storage for all cacheable responses
    set beresp.storage = "persistent";

    # Cache based on status code
    if (beresp.status == 200) {
        # 6 years for successful responses
        set beresp.ttl = 6y;
        set beresp.grace = 1h;
        set beresp.keep = 7y;
        
        # Mark as cacheable
        unset beresp.http.Set-Cookie;
        
        # Add cache control headers
        set beresp.http.Cache-Control = "public, max-age=189216000";
        
    } elsif (beresp.status >= 400 && beresp.status < 600) {
        # 10 minutes for error responses
        set beresp.ttl = 10m;
        set beresp.grace = 30s;
        set beresp.uncacheable = false;
        
    } else {
        # 5 minutes for other non-200 responses (redirects, etc)
        set beresp.ttl = 5m;
        set beresp.grace = 30s;
    }

    # Don't cache if backend says not to
    if (beresp.http.Cache-Control ~ "no-cache" || 
        beresp.http.Cache-Control ~ "no-store" || 
        beresp.http.Cache-Control ~ "private") {
        set beresp.uncacheable = true;
        set beresp.ttl = 0s;
        return (deliver);
    }

    return (deliver);
}

sub vcl_deliver {
    # Add cache hit/miss header for debugging
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
        set resp.http.X-Cache-Hits = obj.hits;
    } else {
        set resp.http.X-Cache = "MISS";
    }

    # Remove internal headers before sending to client
    unset resp.http.X-Product-Group;
    
    # Add age header
    set resp.http.Age = resp.http.Age;
    
    return (deliver);
}

sub vcl_purge {
    return (synth(200, "Purged"));
}

sub vcl_backend_error {
    # Cache backend errors for short time
    set beresp.ttl = 5m;
    set beresp.grace = 1h;
    
    return (deliver);
}