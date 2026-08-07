from __future__ import annotations

import unittest

from herald.links import arxiv_pdf_url


class LinkTests(unittest.TestCase):
    def test_arxiv_abstract_becomes_pdf_url(self) -> None:
        self.assertEqual(
            arxiv_pdf_url("https://arxiv.org/abs/2608.01234v2"),
            "https://arxiv.org/pdf/2608.01234v2",
        )
        self.assertEqual(
            arxiv_pdf_url("https://export.arxiv.org/pdf/hep-th/9901001.pdf"),
            "https://arxiv.org/pdf/hep-th/9901001",
        )

    def test_non_arxiv_url_has_no_pdf_derivation(self) -> None:
        self.assertIsNone(arxiv_pdf_url("https://example.org/paper"))


if __name__ == "__main__":
    unittest.main()

