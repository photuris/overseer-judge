//! Settings read from the environment, and API key lookup.
//!
//! Non-secret settings come from `TYPESAFE_BASE_URL` and
//! `TYPESAFE_DEFAULT_MODEL`. The API key comes from `TYPESAFE_API_KEY`,
//! falling back to a key file (by default `$XDG_CONFIG_HOME/jev` or
//! `~/.config/jev`).

use std::{
    env, fs,
    path::{Path, PathBuf},
};

/// Base URL of the System One API when `TYPESAFE_BASE_URL` is unset.
pub const DEFAULT_BASE_URL: &str = "https://api.typesafe.ai";

/// Model asked when `TYPESAFE_DEFAULT_MODEL` is unset.
pub const DEFAULT_MODEL: &str = "jev-latest";

/// The non-secret settings read from the environment.
#[derive(Debug, Clone, PartialEq)]
pub struct Config {
    /// Base URL of the System One API, without the endpoint path.
    pub base_url: String,
    /// Model filled into requests that do not name one.
    pub model: String,
}

impl Config {
    /// Reads `TYPESAFE_BASE_URL` and `TYPESAFE_DEFAULT_MODEL`.
    ///
    /// A variable that is unset, empty, or not valid Unicode falls back
    /// to [`DEFAULT_BASE_URL`] or [`DEFAULT_MODEL`]. Never fails.
    pub fn from_env() -> Self {
        Self {
            base_url: env_or("TYPESAFE_BASE_URL", DEFAULT_BASE_URL),
            model: env_or("TYPESAFE_DEFAULT_MODEL", DEFAULT_MODEL),
        }
    }
}

/// Returns the value of the variable `name`, or `default` when it is
/// unset, empty, or not valid Unicode.
fn env_or(name: &str, default: &str) -> String {
    env::var(name)
        .ok()
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| default.to_owned())
}

/// No API key was found in the environment or the key file.
#[derive(Debug, thiserror::Error)]
#[error(
    "no API key: set TYPESAFE_API_KEY or write the token to {}",
    key_file.display()
)]
pub struct NoKey {
    /// The key file that was consulted.
    pub key_file: PathBuf,
}

/// Returns the API key.
///
/// `env_key` is the raw `TYPESAFE_API_KEY` value; the caller reads the
/// environment. Its trimmed value wins when non-empty; otherwise the
/// trimmed contents of `key_file` are used when non-empty. An
/// unreadable key file counts as empty.
///
/// # Errors
///
/// Returns [`NoKey`] when both sources are missing or blank.
pub fn key(env_key: Option<&str>, key_file: &Path) -> Result<String, NoKey> {
    if let Some(key) = env_key.map(str::trim).filter(|key| !key.is_empty()) {
        return Ok(key.to_owned());
    }

    fs::read_to_string(key_file)
        .ok()
        .map(|contents| contents.trim().to_owned())
        .filter(|key| !key.is_empty())
        .ok_or_else(|| NoKey {
            key_file: key_file.to_owned(),
        })
}

/// Returns the default key file path.
///
/// That is `$XDG_CONFIG_HOME/jev` when the variable is set and
/// non-empty, else `<home>/.config/jev`, else the bare relative path
/// `jev` when no home directory is known.
pub fn default_key_file() -> PathBuf {
    if let Some(dir) = env::var_os("XDG_CONFIG_HOME").filter(|d| !d.is_empty())
    {
        return PathBuf::from(dir).join("jev");
    }

    env::home_dir().map_or_else(
        || PathBuf::from("jev"),
        |home| home.join(".config").join("jev"),
    )
}

#[cfg(test)]
mod tests {
    use std::fs;

    use tempfile::TempDir;

    use super::*;

    /// Writes `contents` to a key file in a fresh temp dir and returns
    /// the dir (which owns the file's lifetime) and the file's path.
    fn key_file(contents: &str) -> (TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("jev");
        fs::write(&path, contents).unwrap();

        (dir, path)
    }

    #[test]
    fn should_prefer_env_key_when_both_are_set() {
        let (_dir, path) = key_file("file-key\n");

        assert_eq!(key(Some("  env-key \n"), &path).unwrap(), "env-key");
    }

    #[test]
    fn should_read_file_key_when_env_is_empty() {
        let (_dir, path) = key_file("\n  file-key\n");

        assert_eq!(key(Some("  "), &path).unwrap(), "file-key");
    }

    #[test]
    fn should_read_file_key_when_env_is_unset() {
        let (_dir, path) = key_file("file-key");

        assert_eq!(key(None, &path).unwrap(), "file-key");
    }

    #[test]
    fn should_return_no_key_when_both_are_blank() {
        let (_dir, path) = key_file(" \n\t");

        let err = key(Some(""), &path).unwrap_err();

        assert_eq!(
            err.to_string(),
            format!(
                "no API key: set TYPESAFE_API_KEY or write the token to {}",
                path.display()
            )
        );
    }

    #[test]
    fn should_return_no_key_when_file_is_missing() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("absent");

        let err = key(None, &path).unwrap_err();

        assert_eq!(err.key_file, path);
    }

    #[test]
    fn should_follow_environment_when_building_default_key_file() {
        let want = match env::var_os("XDG_CONFIG_HOME") {
            Some(dir) if !dir.is_empty() => PathBuf::from(dir).join("jev"),
            _ => env::home_dir()
                .map_or_else(|| "jev".into(), |h| h.join(".config/jev")),
        };

        assert_eq!(default_key_file(), want);
    }

    #[test]
    fn should_follow_environment_when_loading_config() {
        let want_url = env::var("TYPESAFE_BASE_URL")
            .ok()
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| DEFAULT_BASE_URL.into());
        let want_model = env::var("TYPESAFE_DEFAULT_MODEL")
            .ok()
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| DEFAULT_MODEL.into());

        assert_eq!(
            Config::from_env(),
            Config {
                base_url: want_url,
                model: want_model
            }
        );
    }
}
