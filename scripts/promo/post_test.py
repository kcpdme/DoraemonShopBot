#!/usr/bin/env python3
import json
import tempfile
import unittest
from pathlib import Path

from post import CTA, HEADER, format_promo, load_products, parse_group, parse_groups


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


if __name__ == "__main__":
    unittest.main()
