use axum::{
    Router,
    body::Body,
    http::{StatusCode, header},
    response::{IntoResponse, Redirect, Response},
    routing::get,
};
use std::net::SocketAddr;
use tokio::time::{Duration, sleep};

#[tokio::main]
async fn main() {
    let app = Router::new()
        .route("/", get(home))
        .route("/page2", get(page_two))
        .route("/page3", get(page_three))
        .route("/missing", get(|| async { StatusCode::NOT_FOUND }))
        .route("/gone", get(|| async { StatusCode::GONE }))
        .route("/redirect", get(|| async { Redirect::temporary("/page2") }))
        .route("/loop-a", get(|| async { Redirect::temporary("/loop-b") }))
        .route("/loop-b", get(|| async { Redirect::temporary("/loop-a") }))
        .route("/restricted", get(|| async { StatusCode::FORBIDDEN }))
        .route(
            "/rate-limited",
            get(|| async { StatusCode::TOO_MANY_REQUESTS }),
        )
        .route("/slow", get(slow))
        .route("/private/hidden", get(|| async { "robots disallowed" }))
        .route(
            "/robots.txt",
            get(|| async { "User-agent: SitePaladin\nDisallow: /private\n" }),
        )
        .route("/non-html", get(non_html))
        .route("/oversized", get(oversized));
    let address = SocketAddr::from(([127, 0, 0, 1], 4173));
    let listener = tokio::net::TcpListener::bind(address)
        .await
        .expect("fixture port 4173 must be available");
    println!("Local SitePaladin fixture listening at http://{address}");
    axum::serve(listener, app)
        .await
        .expect("fixture server failed");
}

async fn home() -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
        r##"<!doctype html>
<html lang="en"><head>
<title>SitePaladin fixture: a healthy and useful home page</title>
<meta name="description" content="A complete fixture page with healthy metadata and links for local SitePaladin testing.">
<link rel="canonical" href="/">
<meta property="og:title" content="SitePaladin local fixture">
<meta property="og:description" content="Healthy and problematic pages for a local scan.">
<meta property="og:url" content="/">
<meta property="og:image" content="/preview.png">
</head><body><h1>Local fixture site</h1>
<a href="/page2">Page two</a><a href="/missing">Broken link</a>
<a href="/redirect">Working redirect</a><a href="/loop-a">Redirect loop</a>
<a href="/restricted">Restricted link</a><a href="/rate-limited">Rate-limited link</a>
<a href="/slow">Slow link</a><a href="/non-html">Non-HTML response</a><a href="/oversized">Oversized response</a>
<a href="/private/hidden">Robots-excluded page</a><a href="/?q=one">Query variant</a>
<a href="/?q=two">Query variant two</a><a href="/?q=three">Query variant three</a>
<a href="/?q=four">Query trap</a><a href="/page3">Duplicate description page</a>
<a href="mailto:hello@example.test">Email</a><a href="#intro">Fragment</a>
<p id="intro">No fragment-target validation is performed.</p></body></html>"##,
    )
}

async fn page_two() -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
        r#"<!doctype html>
<html><head>
<base href="/docs/">
<title>SitePaladin fixture: a healthy and useful home page</title>
<title>Duplicate title fixture</title>
<meta name="description" content="">
<meta name="description" content="Duplicate description tag">
<link rel="canonical" href="../page3">
<link rel="canonical" href="/other-canonical">
<meta property="og:image" content="/first.png">
<meta property="og:image" content="/second.png">
</head><body><h1>Page with metadata findings</h1>
<a href="../missing">Relative broken link</a><a href="next?source=fixture">Base URL resolution</a>
<a href="/private/hidden">Robots-excluded link</a>
</body></html>"#,
    )
}

async fn page_three() -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
        r#"<!doctype html>
<html><head>
<title>Page three with its own distinct title</title>
<meta name="description" content="A complete fixture page with healthy metadata and links for local SitePaladin testing.">
<link rel="canonical" href="https://example.test/intentional-canonical-target">
</head><body><h1>Intentional cross-domain canonical</h1></body></html>"#,
    )
}

async fn slow() -> impl IntoResponse {
    sleep(Duration::from_secs(3)).await;
    "This response is intentionally slow."
}

async fn non_html() -> Response {
    (
        [(header::CONTENT_TYPE, "application/octet-stream")],
        b"fixture bytes".to_vec(),
    )
        .into_response()
}

async fn oversized() -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/html")
        .body(Body::from(vec![b'x'; 3_000_000]))
        .expect("fixture response is valid")
}
