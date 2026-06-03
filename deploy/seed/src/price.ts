export function total(cents: number[], discountPercent = 0): number {
  const sum = cents.reduce((a, b) => a + b, 0);
  return Math.round((sum * (100 - discountPercent)) / 100);
}
