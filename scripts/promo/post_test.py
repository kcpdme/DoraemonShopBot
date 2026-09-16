#!/usr/bin/env python3
import json
import os
import tempfile
import unittest
from pathlib import Path

from post import CTA, HEADER, format_promo, load_products, mark_posted, parse_group, parse_groups, seconds_until_due


class ParseGroupTest(unittest.TestCase):
    def test_accepts_urls_and_handles(self):
        self.assertEqual(parse_group("t.me/ChatgptPlusDeal"), "ChatgptPlusDeal")
        self.assertEqual(parse_group("https://t.me/ChatgptPlusDeal"), "ChatgptPlusDeal")
        self.assertEqual(parse_group("@ChatgptPlusDeal"), "ChatgptPlusDeal")
        self.assertEqual(parse_group("ChatgptPlusDeal"), "ChatgptPlusDeal")

    def test_default_groups(self):
        self.assertEqual(parse_groups(None), ["ChatgptPlusDeal"])
        self.assertEqual(parse_groups("t.me/ChatgptPlusDeal, @ChatgptPlusDeal, ChatgptPlusDeal"), ["ChatgptPlusDeal"])


class CatalogTest(unittest.TestCase):
    def test_loads_active_products_and_skips_payloads(self):
        payload = {
            "Products": {
                "PLUS": {
                    "SKU": "PLUS",
                    "Name": "ChatGPT Plus",
                    "Description": "secret pitch",
                    "DeliveryInstruction": "do not leak",
                    "PriceUSDT": 8.5,
                    "Active": True,
                    "CreatedAt": "2026-09-17T10:00:00Z",
                },
                "OLD": {
                    "SKU": "OLD",
                    "Name": "Retired",
                    "PriceUSDT": 1,
                    "Active": False,
                    "CreatedAt": "2026-01-01T00:00:00Z",
                },
                "GONE": {
                    "SKU": "GONE",
                    "Name": "Sold Out Item",
                    "PriceUSDT": 9,
                    "Active": True,
                    "CreatedAt": "2026-09-16T00:00:00Z",
                },
            },
            "Stock": {
                "s1": {"ID": "s1", "SKU": "PLUS", "Payload": "SUPER-SECRET", "Sold": False, "OrderID": ""},
                "s2": {"ID": "s2", "SKU": "PLUS", "Payload": "also-secret", "Sold": True, "OrderID": "o1"},
                "s3": {"ID": "s3", "SKU": "GONE", "Payload": "empty-now", "Sold": True, "OrderID": "o2"},
            },
        }
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "store.json"
            path.write_text(json.dumps(payload), encoding="utf-8")
            products = load_products(path)
        self.assertEqual(len(products), 1)
        self.assertEqual(products[0]["name"], "ChatGPT Plus")
        self.assertEqual(products[0]["stock"], 1)
        self.assertEqual(products[0]["price"], 8.5)
        text = format_promo(products)
        self.assertIn("Zenitsu Thunder Shop", text)
        self.assertTrue(text.startswith(HEADER))
        self.assertIn("ChatGPT Plus", text)
        self.assertIn("$8.50 USDT", text)
        self.assertIn(CTA, text)
        self.assertNotIn("Now available", text)
        self.assertNotIn("SUPER-SECRET", text)
        self.assertNotIn("secret pitch", text)
        self.assertNotIn("do not leak", text)
        self.assertNotIn("Retired", text)
        self.assertNotIn("Sold Out Item", text)
        self.assertNotIn("sold out", text.lower())

    def test_skips_sold_out_and_empty_catalog(self):
        self.assertEqual(format_promo([]), "")
        payload = {
            "Products": {
                "GONE": {
                    "SKU": "GONE",
                    "Name": "Sold Out Item",
                    "PriceUSDT": 9,
                    "Active": True,
                    "CreatedAt": "2026-09-16T00:00:00Z",
                }
            },
            "Stock": {},
        }
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "store.json"
            path.write_text(json.dumps(payload), encoding="utf-8")
            products = load_products(path)
        self.assertEqual(products, [])
        self.assertEqual(format_promo(products), "")


class ScheduleTest(unittest.TestCase):
    def test_due_when_never_posted(self):
        self.assertEqual(seconds_until_due({}), 0)

    def test_waits_for_chosen_interval(self):
        state = {"last_post_unix": 1000, "next_interval": 240}
        self.assertEqual(seconds_until_due(state, now=1100), 140)
        self.assertEqual(seconds_until_due(state, now=1240), 0)
        self.assertLess(seconds_until_due(state, now=1300), 0)

    def test_mark_posted_picks_interval_in_bounds(self):
        os.environ["PROMO_MIN_SECONDS"] = "180"
        os.environ["PROMO_MAX_SECONDS"] = "300"
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "promo.schedule"
            wait = mark_posted(path, now=1_000_000)
            self.assertGreaterEqual(wait, 180)
            self.assertLessEqual(wait, 300)
            remaining = seconds_until_due(json.loads(path.read_text()), now=1_000_000)
            self.assertEqual(remaining, wait)
        os.environ.pop("PROMO_MIN_SECONDS", None)
        os.environ.pop("PROMO_MAX_SECONDS", None)


if __name__ == "__main__":
    unittest.main()
