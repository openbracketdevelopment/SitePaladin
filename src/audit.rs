use scraper::{Html, Selector};
use serde::Serialize;
use url::Url;

#[derive(Clone, Debug, Serialize)]
pub struct Finding {
    pub rule_id: String,
    pub category: String,
    pub severity: String,
    pub status: String,
    pub evidence: String,
    pub explanation: String,
    pub suggestion: String,
}

#[derive(Clone, Debug, Default)]
pub struct Metadata {
    pub title: String,
    pub description: String,
    pub canonical: String,
    pub og_title: String,
    pub og_description: String,
    pub og_url: String,
    pub og_image: String,
    pub robots: String,
    pub links: Vec<String>,
    pub base_url: Option<String>,
}

pub fn normalize_whitespace(value: &str) -> String {
    value.split_whitespace().collect::<Vec<_>>().join(" ")
}

pub fn parse_html(html: &str, page_url: &Url) -> Result<(Metadata, Vec<Finding>), String> {
    let document = Html::parse_document(html);
    let title_selector = selector("title")?;
    let description_selector = selector("meta[name]")?;
    let canonical_selector = selector("link[rel]")?;
    let og_selector = selector("meta[property]")?;
    let link_selector = selector("a[href]")?;
    let base_selector = selector("base[href]")?;

    let titles = document
        .select(&title_selector)
        .map(|node| normalize_whitespace(&node.text().collect::<Vec<_>>().join(" ")))
        .collect::<Vec<_>>();
    let mut metadata = Metadata {
        title: titles.first().cloned().unwrap_or_default(),
        ..Metadata::default()
    };
    let descriptions = document
        .select(&description_selector)
        .filter(|node| {
            node.value()
                .attr("name")
                .is_some_and(|name| name.eq_ignore_ascii_case("description"))
        })
        .map(|node| node.value().attr("content").unwrap_or_default())
        .map(normalize_whitespace)
        .collect::<Vec<_>>();
    metadata.description = descriptions.first().cloned().unwrap_or_default();
    let canonicals = document
        .select(&canonical_selector)
        .filter(|node| {
            node.value().attr("rel").is_some_and(|rel| {
                rel.split_ascii_whitespace()
                    .any(|token| token.eq_ignore_ascii_case("canonical"))
            })
        })
        .filter_map(|node| node.value().attr("href"))
        .map(str::to_owned)
        .collect::<Vec<_>>();
    metadata.canonical = canonicals.first().cloned().unwrap_or_default();
    let og = document
        .select(&og_selector)
        .filter_map(|node| {
            Some((
                node.value().attr("property")?.to_ascii_lowercase(),
                normalize_whitespace(node.value().attr("content").unwrap_or_default()),
            ))
        })
        .collect::<Vec<_>>();
    metadata.og_title = first_og(&og, "og:title");
    metadata.og_description = first_og(&og, "og:description");
    metadata.og_url = first_og(&og, "og:url");
    metadata.og_image = first_og(&og, "og:image");
    metadata.robots = document
        .select(&description_selector)
        .find(|node| {
            node.value()
                .attr("name")
                .is_some_and(|name| name.eq_ignore_ascii_case("robots"))
        })
        .and_then(|node| node.value().attr("content"))
        .map(normalize_whitespace)
        .unwrap_or_default();

    let base_url = document
        .select(&base_selector)
        .filter_map(|node| node.value().attr("href"))
        .filter_map(|href| page_url.join(href).ok())
        .find(|url| matches!(url.scheme(), "http" | "https"));
    metadata.base_url = base_url.map(|url| url.to_string());
    metadata.links = document
        .select(&link_selector)
        .filter_map(|node| node.value().attr("href").map(str::to_owned))
        .collect();

    let mut findings = Vec::new();
    if titles.is_empty() {
        finding(
            &mut findings,
            "title.missing",
            "metadata",
            "warning",
            "issue",
            "",
            "No title element was found.",
            "Add a concise, page-specific title.",
        );
    } else {
        if metadata.title.is_empty() {
            finding(
                &mut findings,
                "title.empty",
                "metadata",
                "warning",
                "issue",
                "",
                "The title contains only whitespace.",
                "Provide a meaningful title.",
            );
        }
        if titles.len() > 1 {
            finding(
                &mut findings,
                "title.multiple",
                "metadata",
                "warning",
                "issue",
                &titles.len().to_string(),
                "More than one title element was found.",
                "Keep one document title.",
            );
        }
        let length = metadata.title.chars().count();
        if !(30..=60).contains(&length) && !metadata.title.is_empty() {
            finding(
                &mut findings,
                "title.length",
                "metadata",
                "warning",
                "warning",
                &length.to_string(),
                "Title length is outside the 30–60 character review range; this is guidance, not a search-engine requirement.",
                "Review the title for clarity; displayed result width varies.",
            );
        }
    }

    if descriptions.is_empty() {
        finding(
            &mut findings,
            "description.missing",
            "metadata",
            "warning",
            "issue",
            "",
            "No meta description was found.",
            "Add a useful page-specific description.",
        );
    } else {
        if metadata.description.is_empty() {
            finding(
                &mut findings,
                "description.empty",
                "metadata",
                "warning",
                "issue",
                "",
                "The meta description is empty.",
                "Provide a useful description.",
            );
        }
        if descriptions.len() > 1 {
            finding(
                &mut findings,
                "description.multiple",
                "metadata",
                "warning",
                "issue",
                &descriptions.len().to_string(),
                "More than one meta description was found.",
                "Keep one meta description.",
            );
        }
        let length = metadata.description.chars().count();
        if !(70..=160).contains(&length) && !metadata.description.is_empty() {
            finding(
                &mut findings,
                "description.length",
                "metadata",
                "warning",
                "warning",
                &length.to_string(),
                "Description length is outside the 70–160 character review range; this is not a ranking rule.",
                "Review it for clarity and usefulness.",
            );
        }
    }

    if canonicals.is_empty() {
        finding(
            &mut findings,
            "canonical.missing",
            "metadata",
            "warning",
            "warning",
            "",
            "No HTML canonical declaration was found; HTTP Link headers are not inspected.",
            "Review whether this page needs a canonical URL.",
        );
    } else if canonicals.len() > 1 {
        finding(
            &mut findings,
            "canonical.multiple",
            "metadata",
            "warning",
            "issue",
            &canonicals.len().to_string(),
            "More than one HTML canonical declaration was found.",
            "Review the declarations and keep the intended canonical.",
        );
    }
    if let Some(raw) = canonicals.first() {
        if raw.trim().is_empty() {
            finding(
                &mut findings,
                "canonical.empty",
                "metadata",
                "warning",
                "issue",
                raw,
                "The canonical href is empty.",
                "Provide an absolute or relative HTTP(S) canonical URL.",
            );
        } else if let Ok(url) = page_url.join(raw) {
            if !matches!(url.scheme(), "http" | "https") || url.host().is_none() {
                finding(
                    &mut findings,
                    "canonical.invalid",
                    "metadata",
                    "warning",
                    "issue",
                    raw,
                    "The canonical URL is not a valid HTTP(S) URL.",
                    "Use a valid HTTP(S) canonical destination.",
                );
            } else {
                if Url::parse(raw).is_err() {
                    finding(
                        &mut findings,
                        "canonical.relative",
                        "metadata",
                        "info",
                        "warning",
                        raw,
                        "The relative canonical resolves against the page URL.",
                        "Review that the resolved destination is intended.",
                    );
                }
                finding(
                    &mut findings,
                    "canonical.destination",
                    "metadata",
                    "info",
                    "not_checked",
                    raw,
                    "Canonical destination availability was not checked.",
                    "Verify the destination separately if its response status matters.",
                );
                if url == *page_url {
                    finding(
                        &mut findings,
                        "canonical.self",
                        "metadata",
                        "info",
                        "pass",
                        raw,
                        "The canonical resolves to this page URL.",
                        "No action is needed if this is intentional.",
                    );
                } else {
                    finding(
                        &mut findings,
                        "canonical.other",
                        "metadata",
                        "info",
                        "pass",
                        raw,
                        "The canonical points to another URL; this is not inherently incorrect.",
                        "Confirm that the cross-page or cross-domain target is intentional.",
                    );
                }
            }
        } else {
            finding(
                &mut findings,
                "canonical.invalid",
                "metadata",
                "warning",
                "issue",
                raw,
                "The canonical URL could not be resolved.",
                "Use a valid HTTP(S) canonical destination.",
            );
        }
    }

    for (property, value) in [
        ("og:title", &metadata.og_title),
        ("og:description", &metadata.og_description),
        ("og:url", &metadata.og_url),
        ("og:image", &metadata.og_image),
    ] {
        if value.is_empty() {
            let id = property.replace(':', ".");
            finding(
                &mut findings,
                &format!("{id}.missing"),
                "social",
                "info",
                "warning",
                "",
                &format!("{property} is missing or empty; this is a sharing-quality observation."),
                "Add the value if this page should have a social sharing preview.",
            );
        }
    }
    if !metadata.og_url.is_empty() && page_url.join(&metadata.og_url).is_err() {
        finding(
            &mut findings,
            "og.url.invalid",
            "social",
            "warning",
            "issue",
            &metadata.og_url,
            "The og:url value could not be resolved.",
            "Use a valid HTTP(S) URL.",
        );
    }
    if !metadata.og_url.is_empty() {
        finding(
            &mut findings,
            "og.url.destination",
            "social",
            "info",
            "not_checked",
            &metadata.og_url,
            "Open Graph URL destination availability was not checked.",
            "Verify the destination separately if its response status matters.",
        );
    }
    if !metadata.og_image.is_empty() && page_url.join(&metadata.og_image).is_err() {
        finding(
            &mut findings,
            "og.image.invalid",
            "social",
            "warning",
            "issue",
            &metadata.og_image,
            "The og:image value could not be resolved.",
            "Use a valid HTTP(S) URL.",
        );
    }
    if !metadata.og_image.is_empty() {
        finding(
            &mut findings,
            "og.image.destination",
            "social",
            "info",
            "not_checked",
            &metadata.og_image,
            "Open Graph image destination availability was not checked.",
            "Verify the image separately if its response status matters.",
        );
    }
    if !metadata.robots.is_empty() {
        finding(
            &mut findings,
            "robots.directive",
            "indexing",
            "info",
            "pass",
            &metadata.robots,
            "A meta robots directive is present; indexing directives are not comprehensively checked.",
            "Confirm that the directive matches your intent.",
        );
    }

    Ok((metadata, findings))
}

fn selector(pattern: &str) -> Result<Selector, String> {
    Selector::parse(pattern).map_err(|error| format!("internal CSS selector error: {error}"))
}

fn first_og(values: &[(String, String)], property: &str) -> String {
    values
        .iter()
        .find(|(key, _)| key == property)
        .map(|(_, value)| value.clone())
        .unwrap_or_default()
}

#[allow(clippy::too_many_arguments)]
fn finding(
    findings: &mut Vec<Finding>,
    rule_id: &str,
    category: &str,
    severity: &str,
    status: &str,
    evidence: &str,
    explanation: &str,
    suggestion: &str,
) {
    findings.push(Finding {
        rule_id: rule_id.to_owned(),
        category: category.to_owned(),
        severity: severity.to_owned(),
        status: status.to_owned(),
        evidence: evidence.to_owned(),
        explanation: explanation.to_owned(),
        suggestion: suggestion.to_owned(),
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn title_length_counts_unicode_characters_after_whitespace_normalization() {
        let title = "é".repeat(29);
        let html = format!(
            "<title> {title} </title><meta name='description' content='{}'>",
            "d".repeat(75)
        );
        let (metadata, findings) =
            parse_html(&html, &Url::parse("https://example.com/").unwrap()).unwrap();
        assert_eq!(metadata.title.chars().count(), 29);
        assert!(findings.iter().any(|f| f.rule_id == "title.length"));
    }

    #[test]
    fn detects_missing_empty_multiple_and_relative_canonical_metadata() {
        let html = "<title>Example</title><title>Other</title><meta name='description' content=''><link rel='canonical' href='../target'><link rel='canonical' href='/other'>";
        let (_, findings) =
            parse_html(html, &Url::parse("https://example.com/a/page").unwrap()).unwrap();
        for id in ["title.multiple", "description.empty", "canonical.multiple"] {
            assert!(findings.iter().any(|f| f.rule_id == id), "missing {id}");
        }
    }

    #[test]
    fn resolves_first_valid_base_for_links() {
        let html = "<base href='https://example.com/docs/'><a href='../other'>link</a>";
        let (metadata, _) =
            parse_html(html, &Url::parse("https://example.com/start").unwrap()).unwrap();
        assert_eq!(metadata.links, ["../other"]);
        assert_eq!(
            Url::parse("https://example.com/docs/")
                .unwrap()
                .join(&metadata.links[0])
                .unwrap()
                .as_str(),
            "https://example.com/other"
        );
    }
}
