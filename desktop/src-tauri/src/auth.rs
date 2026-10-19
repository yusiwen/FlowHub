//! dsh web launch-token handling.
//!
//! dsh accepts `GET /?token=<launch token>` exactly once, answers `303` and sets
//! an HttpOnly `dsh-auth-<authority hash>` cookie. The token is random per dsh
//! process and is never persisted; the cookie is the session and lasts 30 days.

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use sha2::{Digest, Sha256};
use url::Url;

pub const DSH_COOKIE_PREFIX: &str = "dsh-auth-";

/// Accept either the whole URL printed by `dsh web` or a bare token. A pasted
/// URL must point at the same origin as the instance, otherwise the token is
/// almost certainly from another server.
pub fn parse_token_input(input: &str, base: &Url) -> Result<String, String> {
    let input = input.trim();
    if input.is_empty() {
        return Err("paste the URL printed by dsh web, or the token itself".to_string());
    }

    if input.contains("://") {
        let url = Url::parse(input).map_err(|error| format!("that is not a valid URL: {error}"))?;
        if !matches!(url.scheme(), "http" | "https") {
            return Err("the URL must use http or https".to_string());
        }
        if !same_origin(&url, base) {
            return Err(format!(
                "that URL is for {}, not {}",
                url.origin().ascii_serialization(),
                base.origin().ascii_serialization()
            ));
        }
        let tokens: Vec<String> = url
            .query_pairs()
            .filter(|(key, _)| key == "token")
            .map(|(_, value)| value.into_owned())
            .collect();
        if tokens.len() != 1 {
            return Err("the URL must carry exactly one token parameter".to_string());
        }
        let token = tokens[0].trim();
        if token.is_empty() {
            return Err("the token is empty".to_string());
        }
        check_token(token)?;
        Ok(token.to_string())
    } else {
        check_token(input)?;
        Ok(input.to_string())
    }
}

fn check_token(token: &str) -> Result<(), String> {
    if token.chars().any(char::is_whitespace) {
        return Err("the token must not contain whitespace".to_string());
    }
    if token.len() > 512 {
        return Err("the token is too long".to_string());
    }
    Ok(())
}

/// Build the exchange URL. dsh only accepts the root path with exactly one
/// `token` parameter; unrelated query parameters are preserved.
pub fn token_url(base: &Url, token: &str) -> Url {
    let mut url = base.clone();
    url.set_path("/");
    url.set_fragment(None);
    let kept: Vec<(String, String)> = url
        .query_pairs()
        .filter(|(key, _)| key != "token")
        .map(|(key, value)| (key.into_owned(), value.into_owned()))
        .collect();
    url.set_query(None);
    {
        let mut query = url.query_pairs_mut();
        for (key, value) in kept {
            query.append_pair(&key, &value);
        }
        query.append_pair("token", token);
    }
    url
}

pub fn same_origin(candidate: &Url, base: &Url) -> bool {
    candidate.scheme() == base.scheme()
        && candidate.host_str() == base.host_str()
        && candidate.port_or_known_default() == base.port_or_known_default()
}

/// dsh names its session cookie `dsh-auth-<base64url(sha256(authority))>`, so a
/// cookie from another dsh port on the same host cannot be mistaken for this
/// instance. Ports are omitted only when the URL uses the scheme default.
pub fn cookie_name(base: &Url) -> String {
    let host = base.host_str().unwrap_or_default();
    let host = if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]")
    } else {
        host.to_string()
    };
    let authority = match base.port() {
        Some(port) => format!("{host}:{port}"),
        None => host,
    };
    let digest = Sha256::digest(authority.as_bytes());
    format!("{DSH_COOKIE_PREFIX}{}", URL_SAFE_NO_PAD.encode(digest))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base() -> Url {
        Url::parse("http://127.0.0.1:3080/").expect("base url")
    }

    #[test]
    fn a_bare_token_is_accepted() {
        assert_eq!(
            parse_token_input("abc_DEF-123", &base()).expect("token"),
            "abc_DEF-123"
        );
        assert_eq!(
            parse_token_input("  abc  ", &base()).expect("trimmed"),
            "abc"
        );
    }

    #[test]
    fn the_full_printed_url_is_accepted() {
        let token =
            parse_token_input("http://127.0.0.1:3080/?token=abc_DEF-123", &base()).expect("token");
        assert_eq!(token, "abc_DEF-123");
    }

    #[test]
    fn another_origin_is_rejected() {
        let error =
            parse_token_input("http://127.0.0.1:3081/?token=abc", &base()).expect_err("wrong port");
        assert!(error.contains("not http://127.0.0.1:3080"), "{error}");

        let error =
            parse_token_input("http://localhost:3080/?token=abc", &base()).expect_err("wrong host");
        assert!(error.contains("not http://127.0.0.1:3080"), "{error}");
    }

    #[test]
    fn a_url_without_exactly_one_token_is_rejected() {
        assert!(parse_token_input("http://127.0.0.1:3080/", &base()).is_err());
        assert!(parse_token_input("http://127.0.0.1:3080/?token=a&token=b", &base()).is_err());
        assert!(parse_token_input("http://127.0.0.1:3080/?token=", &base()).is_err());
    }

    #[test]
    fn bare_tokens_reject_whitespace_and_absurd_length() {
        assert!(parse_token_input("abc def", &base()).is_err());
        assert!(parse_token_input(&"x".repeat(513), &base()).is_err());
    }

    #[test]
    fn token_url_uses_the_root_and_keeps_one_token() {
        let base = Url::parse("http://127.0.0.1:3080/ui?foo=1&token=old").expect("url");
        let url = token_url(&base, "new");
        assert_eq!(url.path(), "/");
        assert_eq!(
            url.query_pairs().filter(|(key, _)| key == "token").count(),
            1
        );
        assert_eq!(
            url.query_pairs()
                .find(|(key, _)| key == "token")
                .map(|(_, value)| value.into_owned()),
            Some("new".to_string())
        );
        assert_eq!(
            url.query_pairs()
                .find(|(key, _)| key == "foo")
                .map(|(_, value)| value.into_owned()),
            Some("1".to_string())
        );
    }

    #[test]
    fn cookie_name_matches_the_dsh_authority_hash() {
        let base = Url::parse("http://127.0.0.1:3080/").expect("url");
        assert_eq!(
            cookie_name(&base),
            "dsh-auth-VPhEEcLKeqRDBoBalzN2Nm7CnfxKhLE00pKIDWxt1sw"
        );
        let other_port = Url::parse("http://127.0.0.1:3081/").expect("url");
        assert_ne!(cookie_name(&base), cookie_name(&other_port));
    }
}
