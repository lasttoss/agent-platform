"""The guard's job: an obvious mutation never travels, and a semicolon in data is not two queries."""
import sys
import pathlib
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import sqlguard  # noqa: E402

ALLOWED = [
    "SELECT 1",
    "select count() from events",
    "SELECT * FROM analytics.events LIMIT 10",
    "WITH daily AS (SELECT toDate(ts) d, count() n FROM events GROUP BY d) SELECT * FROM daily",
    "SELECT 'a;b' AS punctuation",
    "SELECT 1 -- a comment\n",
    "/* nothing to see */ SELECT 1",
    "SELECT * FROM events WHERE what = 'DROP TABLE events'",
    "SELECT 1;",
]

REFUSED = {
    "DROP TABLE events": "keyword",
    "INSERT INTO events VALUES (1)": "keyword",
    "ALTER TABLE events DELETE WHERE 1": "keyword",
    "UPDATE events SET a = 1": "keyword",
    "SELECT 1; DROP TABLE events": "one statement",
    "SELECT 1; SELECT 2": "one statement",
    "SELECT * FROM system.users": "catalogue",
    "SELECT * FROM system.processes": "catalogue",
    "SELECT * FROM events INTO OUTFILE '/tmp/x'": "OUTFILE",
    "SELECT * FROM events FORMAT CSV": "FORMAT",
    "SELECT * FROM events SETTINGS max_rows_to_read = 1": "SETTINGS",
    "TRUNCATE TABLE events": "keyword",
    "OPTIMIZE TABLE events FINAL": "keyword",
    "CREATE TABLE x (a Int8) ENGINE = Memory": "keyword",
    "GRANT SELECT ON *.* TO app": "keyword",
    "": "empty",
    "   ": "empty",
}


class GuardTest(unittest.TestCase):
    def test_allowed(self):
        for sql in ALLOWED:
            verdict = sqlguard.check_readonly(sql)
            self.assertTrue(verdict.ok, f"{sql!r} was refused: {verdict.reason}")

    def test_refused(self):
        for sql, hint in REFUSED.items():
            verdict = sqlguard.check_readonly(sql)
            self.assertFalse(verdict.ok, f"{sql!r} was allowed")
            self.assertIn(hint.lower(), verdict.reason.lower(), f"{sql!r} → {verdict.reason}")

    def test_comment_cannot_hide_a_second_statement(self):
        verdict = sqlguard.check_readonly("SELECT 1 -- harmless\n; DROP TABLE events")
        self.assertFalse(verdict.ok)

    def test_comment_inside_a_string_is_data(self):
        verdict = sqlguard.check_readonly("SELECT '-- not a comment' AS x")
        self.assertTrue(verdict.ok, verdict.reason)

    def test_leading_semicolon(self):
        self.assertFalse(sqlguard.check_readonly("; SELECT 1").ok)

    def test_sql_too_long(self):
        self.assertFalse(sqlguard.check_readonly("SELECT '" + "a" * 8100 + "'").ok)

    def test_non_string(self):
        self.assertFalse(sqlguard.check_readonly(None).ok)

    def test_returns_sql_without_comment_or_trailing_semicolon(self):
        verdict = sqlguard.check_readonly("SELECT 1; -- done")
        self.assertEqual(verdict.sql, "SELECT 1")


if __name__ == "__main__":
    unittest.main()
