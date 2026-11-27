//! Minimal RFC 3339 timestamp parsing for snapshot ages.
//!
//! The fleet crate intentionally has no time-format dependency.

/// Seconds since the Unix epoch for an RFC 3339 timestamp.
pub fn parse_rfc3339(value: &str) -> Option<i64> {
    let value = value.trim();
    let (date, rest) = value.split_once('T')?;
    let mut date_parts = date.split('-');
    let year: i64 = date_parts.next()?.parse().ok()?;
    let month: i64 = date_parts.next()?.parse().ok()?;
    let day: i64 = date_parts.next()?.parse().ok()?;
    if date_parts.next().is_some() || !(1..=12).contains(&month) {
        return None;
    }
    let (clock, offset) = split_offset(rest)?;
    let mut clock_parts = clock.split(':');
    let hour: i64 = clock_parts.next()?.parse().ok()?;
    let minute: i64 = clock_parts.next()?.parse().ok()?;
    let second_part = clock_parts.next()?;
    if clock_parts.next().is_some() {
        return None;
    }
    let second_text = second_part.split('.').next()?;
    let second: i64 = second_text.parse().ok()?;
    if !(0..=23).contains(&hour) || !(0..=59).contains(&minute) || !(0..=60).contains(&second) {
        return None;
    }
    let days = days_from_civil(year, month, day);
    let seconds = days
        .checked_mul(86_400)?
        .checked_add(hour * 3600 + minute * 60 + second)?;
    seconds.checked_sub(offset)
}

fn split_offset(rest: &str) -> Option<(&str, i64)> {
    if let Some(clock) = rest.strip_suffix('Z') {
        return Some((clock, 0));
    }
    if let Some(clock) = rest.strip_suffix("+00:00") {
        return Some((clock, 0));
    }
    let bytes = rest.as_bytes();
    if bytes.len() >= 6 {
        let (clock, offset) = rest.split_at(rest.len() - 6);
        let sign = &offset[..1];
        let (hours, minutes) = offset[1..].split_once(':')?;
        let hours: i64 = hours.parse().ok()?;
        let minutes: i64 = minutes.parse().ok()?;
        if !(0..=23).contains(&hours) || !(0..=59).contains(&minutes) {
            return None;
        }
        let total = hours * 3600 + minutes * 60;
        return match sign {
            "+" => Some((clock, total)),
            "-" => Some((clock, -total)),
            _ => None,
        };
    }
    (rest.len() >= 19).then_some((rest, 0))
}

fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let year = if month <= 2 { year - 1 } else { year };
    let era = if year >= 0 { year } else { year - 399 } / 400;
    let yoe = year - era * 400;
    let doy = (153 * (month + (if month > 2 { -3 } else { 9 })) + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

pub fn now_seconds() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

pub fn format_age(seconds: i64) -> String {
    let seconds = seconds.max(0);
    if seconds < 60 {
        format!("{seconds}s")
    } else if seconds < 3600 {
        format!("{}m", seconds / 60)
    } else if seconds < 86_400 {
        format!("{}h", seconds / 3600)
    } else {
        format!("{}d", seconds / 86_400)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_common_rfc3339_forms() {
        assert_eq!(parse_rfc3339("1970-01-01T00:00:00Z"), Some(0));
        assert_eq!(
            parse_rfc3339("2026-11-27T12:00:00.123Z"),
            Some(1_795_780_800)
        );
        assert_eq!(
            parse_rfc3339("2026-11-27T12:00:00+00:00"),
            Some(1_795_780_800)
        );
        assert_eq!(
            parse_rfc3339("2026-11-27T13:00:00+01:00"),
            Some(1_795_780_800)
        );
        assert_eq!(
            parse_rfc3339("2026-11-27T11:00:00-01:00"),
            Some(1_795_780_800)
        );
    }

    #[test]
    fn rejects_garbage() {
        assert_eq!(parse_rfc3339("not a time"), None);
        assert_eq!(parse_rfc3339("2026-13-01T00:00:00Z"), None);
        assert_eq!(parse_rfc3339("2026-01-01T25:00:00Z"), None);
    }

    #[test]
    fn formats_ages() {
        assert_eq!(format_age(42), "42s");
        assert_eq!(format_age(300), "5m");
        assert_eq!(format_age(7200), "2h");
    }
}
