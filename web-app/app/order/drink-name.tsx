import { orderParts, orderQualifiers } from "./drinks";

// DrinkName shows an order as "Lungo, decaf" or "Iced Latte, oat milk": the
// drink in the surrounding style, the beans and milk after it in a lighter
// tone, so they read as qualifiers on the drink rather than part of its name.
// Regular beans and no milk show the drink alone.
export function DrinkName({
  drink,
  beans,
  milk,
  beansClassName = "text-neutral-400",
}: {
  drink: string;
  beans?: string;
  milk?: string;
  beansClassName?: string;
}) {
  const p = orderParts(drink, beans, milk);
  const q = orderQualifiers(p);
  return (
    <>
      {p.drink}
      {p.drink && q && <span className={beansClassName}>, {q}</span>}
    </>
  );
}
