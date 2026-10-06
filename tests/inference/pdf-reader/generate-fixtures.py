#!/usr/bin/env python3
"""Regenerate synthetic PDF fixtures: reportlab + Pillow, a Cyrillic TTF.

Usage: python3 generate-fixtures.py /path/to/DejaVuSans.ttf
Only regeneration needs these dependencies; the API smoke uses stdlib.
"""
import io
from pathlib import Path
import sys

from PIL import Image, ImageDraw, ImageFont
from reportlab.lib.pdfencrypt import StandardEncryption
from reportlab.lib.utils import ImageReader
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.ttfonts import TTFont
from reportlab.pdfgen import canvas


def main():
    font = sys.argv[1]
    out = Path(__file__).resolve().parent
    pdfmetrics.registerFont(TTFont("Fixture", font))
    for name, encryption in (
        ("text.pdf", None),
        ("locked.pdf", StandardEncryption("fixture-password", strength=128)),
    ):
        pdf = canvas.Canvas(str(out / name), invariant=1, encrypt=encryption)
        pdf.setFont("Fixture", 14)
        for y, line in zip((780, 750, 720), (
            "PDF_READER_PAGE_ONE_7F31",
            "Проверка чтения: договор Север, сумма 12345 рублей.",
            "English text: invoice North, amount 12345 RUB.",
        )):
            pdf.drawString(40, y, line)
        pdf.showPage()
        pdf.setFont("Fixture", 14)
        pdf.drawString(40, 780, "PDF_READER_PAGE_TWO_9C82")
        for y, row in zip((730, 700, 670), (
            ("Товар", "Количество", "Сумма"),
            ("Бумага", "17", "2345"),
            ("Итого", "17", "2345"),
        )):
            for x, value in zip((40, 240, 420), row):
                pdf.drawString(x, y, value)
            pdf.line(40, y - 6, 540, y - 6)
        pdf.save()

    # Image-only page: no invisible text layer that could fake OCR success.
    img = Image.new("RGB", (1500, 500), "white")
    draw = ImageDraw.Draw(img)
    image_font = ImageFont.truetype(font, 48)
    draw.text((50, 80), "SCAN READER 48271", font=image_font, fill="black")
    draw.text((50, 180), "Скан документа: сумма 67890 рублей", font=image_font, fill="black")
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    pdf = canvas.Canvas(str(out / "scan.pdf"), invariant=1)
    pdf.drawImage(ImageReader(buf), 30, 600, width=535, height=178)
    pdf.save()


if __name__ == "__main__":
    main()
